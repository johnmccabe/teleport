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


Keystore implements functions for saving and loading from hard disc
temporary teleport certificates
*/

package client

import (
	"encoding/json"
	"io/ioutil"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/gravitational/teleport/lib/backend/boltbk"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/sshutils"

	log "github.com/Sirupsen/logrus"
	"github.com/gravitational/trace"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// AddHostSignersToCache takes a list of CAs whom we trust. This list is added to a database
// of "seen" CAs.
//
// Every time we connect to a new host, we'll request its certificaate to be signed by one
// of these trusted CAs.
//
// Why do we trust these CAs? Because we received them from a trusted Teleport Proxy.
// Why do we trust the proxy? Because we've connected to it via HTTPS + username + Password + HOTP.
func AddHostSignersToCache(hostSigners []services.CertAuthority) error {
	bk, err := boltbk.New(filepath.Join(getKeysDir(), HostSignersFilename))
	if err != nil {
		return trace.Wrap(nil)
	}
	defer bk.Close()
	ca := services.NewCAService(bk)

	for _, hostSigner := range hostSigners {
		err := ca.UpsertCertAuthority(hostSigner, 0)
		if err != nil {
			return trace.Wrap(nil)
		}
	}
	return nil
}

// CheckHostSignature checks if the given host key was signed by one of the trusted
// certificaate authorities (CAs)
func CheckHostSignature(hostId string, remote net.Addr, key ssh.PublicKey) error {
	cert, ok := key.(*ssh.Certificate)
	if !ok {
		return trace.Errorf("expected certificate")
	}

	bk, err := boltbk.New(filepath.Join(getKeysDir(), HostSignersFilename))
	if err != nil {
		return trace.Wrap(nil)
	}
	defer bk.Close()
	ca := services.NewCAService(bk)

	cas, err := ca.GetCertAuthorities(services.HostCA)
	if err != nil {
		return trace.Wrap(err)
	}

	for i := range cas {
		checkers, err := cas[i].Checkers()
		if err != nil {
			return trace.Wrap(err)
		}
		for _, checker := range checkers {
			if sshutils.KeysEqual(cert.SignatureKey, checker) {
				return nil
			}
		}
	}
	return trace.Errorf("no matching authority found")
}

// GetLocalAgentKeys returns a list of local keys agents can use
// to authenticate
func GetLocalAgentKeys() ([]agent.AddedKey, error) {
	err := initKeysDir()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	existingKeys, err := loadAllKeys()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	addedKeys := make([]agent.AddedKey, len(existingKeys))
	for i, key := range existingKeys {
		pcert, _, _, _, err := ssh.ParseAuthorizedKey(key.Cert)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		pk, err := ssh.ParseRawPrivateKey(key.Priv)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		addedKey := agent.AddedKey{
			PrivateKey:       pk,
			Certificate:      pcert.(*ssh.Certificate),
			Comment:          "",
			LifetimeSecs:     0,
			ConfirmBeforeUse: false,
		}
		addedKeys[i] = addedKey
	}
	return addedKeys, nil
}

// GetLocalAgent loads all the saved teleport certificates and
// creates ssh agent with them
func GetLocalAgent() (agent.Agent, error) {
	keys, err := GetLocalAgentKeys()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	keyring := agent.NewKeyring()
	for _, key := range keys {
		if err := keyring.Add(key); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return keyring, nil
}

func initKeysDir() error {
	_, err := os.Stat(getKeysDir())
	if os.IsNotExist(err) {
		err = os.MkdirAll(getKeysDir(), os.ModeDir|0777)
		if err != nil {
			return trace.Wrap(err)
		}
	} else {
		if err != nil {
			return trace.Wrap(err)
		}
	}
	return nil
}

type Key struct {
	Priv     []byte
	Cert     []byte
	Deadline time.Time
}

func saveKey(key Key, filename string) error {
	err := initKeysDir()
	if err != nil {
		return trace.Wrap(err)
	}
	bytes, err := json.Marshal(key)
	if err != nil {
		return trace.Wrap(err)
	}

	err = ioutil.WriteFile(filename, bytes, 0666)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func loadKey(filename string) (Key, error) {
	bytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return Key{}, trace.Wrap(err)
	}

	var key Key

	err = json.Unmarshal(bytes, &key)
	if err != nil {
		return Key{}, trace.Wrap(err)
	}

	return key, nil

}

func loadAllKeys() ([]Key, error) {
	keys := make([]Key, 0)
	files, err := ioutil.ReadDir(getKeysDir())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	for _, file := range files {
		if !file.IsDir() && strings.HasPrefix(file.Name(), KeyFilePrefix) &&
			strings.HasSuffix(file.Name(), KeyFileSuffix) {
			key, err := loadKey(filepath.Join(getKeysDir(), file.Name()))
			if err != nil {
				log.Errorf(err.Error())
				continue
			}

			if time.Now().Before(key.Deadline) {
				keys = append(keys, key)
			} else {
				// remove old keys
				err = os.Remove(filepath.Join(getKeysDir(), file.Name()))
				if err != nil {
					log.Errorf(err.Error())
				}
			}
		}
	}
	return keys, nil
}

// getKeysDir() returns the directory where a client can store the temporary keys
func getKeysDir() string {
	var baseDir string
	u, err := user.Current()
	if err != nil {
		baseDir = os.TempDir()
	} else {
		baseDir = u.HomeDir
	}
	return filepath.Join(baseDir, ".tsh")
}

var (
	KeyFilePrefix       = "teleport_"
	KeyFileSuffix       = ".tkey"
	HostSignersFilename = "hostsigners.db"
)
