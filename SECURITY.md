# Security policy

OPSEC Market is an alpha that runs on **test networks only**. It has no bounty
programme and makes no promise about response time.

## Report a vulnerability privately

Use GitHub's private vulnerability reporting:
<https://github.com/yegamble/opsecmkt/security/advisories/new>

**Do not open a public issue, pull request or discussion for a vulnerability.**
Include the version or commit, the steps that trigger it, and what an attacker
gains. Do not include real credentials, real funds or other people's data.

## Scope

- In scope: the code in this repository, and deployments that run it on a test
  network.
- Mainnet is refused by design (Bitcoin mainnet and testnet3, and Monero mainnet
  and testnet, are not accepted), and no mainnet deployment is supported. A
  report that needs mainnet is out of scope.
- Out of scope: third-party software (Bitcoin Core, Monero, Tor, PostgreSQL,
  Docker), a misconfigured or unpatched host, attacks that need physical access
  or an already-compromised operator, and reports against someone else's
  deployment of this software.
- Please do not test against a deployment you do not run or have not been given
  permission to test.

## Supported versions

Only the latest prerelease tag listed on the
[Releases page](https://github.com/yegamble/opsecmkt/releases) is supported.
The `main` branch and older tags are not.

## Bounty

There is none. Reporters who want credit are named in the advisory.
