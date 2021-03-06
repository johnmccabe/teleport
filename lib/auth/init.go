/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"

	log "github.com/Sirupsen/logrus"
	"github.com/gravitational/trace"
	"golang.org/x/crypto/ssh"
)

// InitConfig is auth server init config
type InitConfig struct {
	Backend   backend.Backend
	Authority Authority

	// HostUUID is a UUID of this host
	HostUUID string

	// DomainName stores the FQDN of the signing CA (its certificate will have this
	// name embedded). It is usually set to the GUID of the host the Auth service runs on
	DomainName string

	// AuthServiceName is a human-readable name of this CA. If several Auth services are running
	// (managing multiple teleport clusters) this field is used to tell them apart in UIs
	// It usually defaults to the hostname of the machine the Auth service runs on.
	AuthServiceName string

	// DataDir is the full path to the directory where keys, events and logs are kept
	DataDir string

	SecretKey     string
	AllowedTokens map[string]string

	// HostCA is an optional host certificate authority keypair
	HostCA *services.CertAuthority
	// UserCA is an optional user certificate authority keypair
	UserCA *services.CertAuthority
}

// Init instantiates and configures an instance of AuthServer
func Init(cfg InitConfig) (*AuthServer, *Identity, error) {
	if cfg.DataDir == "" {
		return nil, nil, trace.Wrap(teleport.BadParameter("data_dir", "data dir can not be empty"))
	}

	if cfg.HostUUID == "" {
		return nil, nil, trace.Wrap(teleport.BadParameter("HostUUID", "host UUID can not be empty"))
	}

	err := os.MkdirAll(cfg.DataDir, os.ModeDir|0777)
	if err != nil {
		log.Errorf(err.Error())
		return nil, nil, err
	}

	lockService := services.NewLockService(cfg.Backend)
	err = lockService.AcquireLock(cfg.DomainName, 60*time.Second)
	if err != nil {
		return nil, nil, err
	}
	defer lockService.ReleaseLock(cfg.DomainName)

	// check that user CA and host CA are present and set the certs if needed
	asrv := NewAuthServer(&cfg)

	// we determine if it's the first start by checking if the CA's are set
	var firstStart bool

	// this block will generate user CA authority on first start if it's
	// not currently present, it will also use optional passed user ca keypair
	// that can be supplied in configuration
	if _, err := asrv.GetCertAuthority(services.CertAuthID{DomainName: cfg.DomainName, Type: services.HostCA}, false); err != nil {
		if !teleport.IsNotFound(err) {
			return nil, nil, trace.Wrap(err)
		}
		firstStart = true
		if cfg.HostCA == nil {
			log.Infof("FIRST START: Generating host CA on first start")
			priv, pub, err := asrv.GenerateKeyPair("")
			if err != nil {
				return nil, nil, trace.Wrap(err)
			}
			cfg.HostCA = &services.CertAuthority{
				DomainName:   cfg.DomainName,
				Type:         services.HostCA,
				SigningKeys:  [][]byte{priv},
				CheckingKeys: [][]byte{pub},
			}
		}
		if err := asrv.CAService.UpsertCertAuthority(*cfg.HostCA, backend.Forever); err != nil {
			return nil, nil, trace.Wrap(err)
		}
	}

	// this block will generate user CA authority on first start if it's
	// not currently present, it will also use optional passed user ca keypair
	// that can be supplied in configuration
	if _, err := asrv.GetCertAuthority(services.CertAuthID{DomainName: cfg.DomainName, Type: services.UserCA}, false); err != nil {
		if !teleport.IsNotFound(err) {
			return nil, nil, trace.Wrap(err)
		}
		firstStart = true
		if cfg.UserCA == nil {
			log.Infof("FIRST START: Generating user CA on first start")
			priv, pub, err := asrv.GenerateKeyPair("")
			if err != nil {
				return nil, nil, trace.Wrap(err)
			}
			cfg.UserCA = &services.CertAuthority{
				DomainName:   cfg.DomainName,
				Type:         services.UserCA,
				SigningKeys:  [][]byte{priv},
				CheckingKeys: [][]byte{pub},
			}
		}
		if err := asrv.CAService.UpsertCertAuthority(*cfg.UserCA, backend.Forever); err != nil {
			return nil, nil, trace.Wrap(err)
		}
	}
	if firstStart {
		if len(cfg.AllowedTokens) != 0 {
			log.Infof("FIRST START: Setting allowed provisioning tokens")
			for token, domainName := range cfg.AllowedTokens {
				log.Infof("FIRST START: upsert provisioning token: domainName: %v", domainName)
				var role string
				token, role, err = services.SplitTokenRole(token)
				if err != nil {
					return nil, nil, trace.Wrap(err)
				}

				if err := asrv.UpsertToken(token, role, 600*time.Second); err != nil {
					return nil, nil, trace.Wrap(err)
				}
			}
		}
	}

	identity, err := initKeys(asrv, cfg.DataDir, IdentityID{HostUUID: cfg.HostUUID, Role: teleport.RoleAdmin})
	if err != nil {
		return nil, nil, err
	}

	return asrv, identity, nil
}

// initKeys initializes this node's host certificate signed by host authority
func initKeys(a *AuthServer, dataDir string, id IdentityID) (*Identity, error) {
	kp, cp := keysPath(dataDir, id)

	keyExists, err := pathExists(kp)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	certExists, err := pathExists(cp)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if !keyExists || !certExists {
		privateKey, publicKey, err := a.GenerateKeyPair("")
		if err != nil {
			return nil, trace.Wrap(err)
		}
		cert, err := a.GenerateHostCert(publicKey, id.HostUUID, a.DomainName, id.Role, 0)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if err := writeKeys(dataDir, id, privateKey, cert); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	i, err := ReadIdentity(dataDir, id)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return i, nil
}

// writeKeys saves the key/cert pair for a given domain onto disk. This usually means the
// domain trusts us (signed our public key)
func writeKeys(dataDir string, id IdentityID, key []byte, cert []byte) error {
	kp, cp := keysPath(dataDir, id)
	log.Debugf("write key to %v, cert from %v", kp, cp)

	if err := ioutil.WriteFile(kp, key, 0600); err != nil {
		return err
	}
	if err := ioutil.WriteFile(cp, cert, 0600); err != nil {
		return err
	}
	return nil
}

// Identity is a collection of certificates and signers that represent identity
type Identity struct {
	KeyBytes  []byte
	CertBytes []byte
	KeySigner ssh.Signer
	PubKey    ssh.PublicKey
	Cert      *ssh.Certificate
}

// IdentityID is a combination of role and host UUID
type IdentityID struct {
	Role     teleport.Role
	HostUUID string
}

// ReadIdentity reads, parses and returns the given pub/pri key + cert from the
// key storage (dataDir).
func ReadIdentity(dataDir string, id IdentityID) (i *Identity, err error) {
	kp, cp := keysPath(dataDir, id)
	log.Debugf("host identity: [key: %v, cert: %v]", kp, cp)

	i = &Identity{}

	i.KeyBytes, err = utils.ReadPath(kp)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	i.CertBytes, err = utils.ReadPath(cp)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	i.PubKey, _, _, _, err = ssh.ParseAuthorizedKey(i.CertBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse server CA certificate '%v', err: %v",
			string(i.CertBytes), err)
	}

	var ok bool
	i.Cert, ok = i.PubKey.(*ssh.Certificate)
	if !ok {
		return nil, fmt.Errorf("expected CA certificate, got %T ", i.PubKey)
	}

	signer, err := ssh.ParsePrivateKey(i.KeyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse host private key, err: %v", err)
	}
	// TODO: why NewCertSigner if we already have a signer from ParsePrivateKey?
	i.KeySigner, err = ssh.NewCertSigner(i.Cert, signer)
	if err != nil {
		return nil, fmt.Errorf("failed to parse host private key, err: %v", err)
	}
	return i, nil
}

// HaveHostKeys checks either the host keys are in place
func HaveHostKeys(dataDir string, id IdentityID) (bool, error) {
	kp, cp := keysPath(dataDir, id)

	exists, err := pathExists(kp)
	if !exists || err != nil {
		return exists, err
	}

	exists, err = pathExists(cp)
	if !exists || err != nil {
		return exists, err
	}

	return true, nil
}

// keysPath returns two full file paths: to the host.key and host.cert
func keysPath(dataDir string, id IdentityID) (key string, cert string) {
	return filepath.Join(dataDir, fmt.Sprintf("host.%v.%v.key", id.HostUUID, string(id.Role))),
		filepath.Join(dataDir, fmt.Sprintf("host.%v.%v.cert", id.HostUUID, string(id.Role)))
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
