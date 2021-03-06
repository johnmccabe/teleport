# FAQ

**Can I use Gravitational Teleport instead of OpenSSH in production today?**

!!! danger "IMPORTANT": 
    At this time Teleport is NOT recommended for production use, but the code is open and 
    available for your security team to inspect. Currently Teleport is undergoing an independent 
    security review. We will be more comfortable recommending it for production use once the 
    review will have completed.

**Can I use OpenSSH client to connect to servers in a Teleport cluster?**

Yes. Take a look at [Using OpenSSH client](user-manual.md#integration-with-openssh) section in the User Manual
and [Using OpenSSH servers](admin-guide.md) in the Admin Manual.

**Which TCP ports does Teleport uses?**

[Ports](admin-guide.md#ports) section of the Admin Manual covers it.

**Do you offer commercial support for Teleport?**

Yes, we plan on offering commercial support soon which will include:

* Commercial version of Teleport which includes multi-cluster capabilities, 
  integration with enterprise identity management (LDAP and others) and custom features.
* Option of fully managed Teleport clusters running on your infrastructure.
* Commercial support with guaranteed response times.

Reach out to `sales@gravitational.com` if you have questions about commerial edition of Teleport.
