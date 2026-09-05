# Apple fixtures

Original project fixtures, generated from `fixture.c` and minimal plists with
Apple's command-line tools. They contain no vendor application code.

- `Fixture.app` has an ad-hoc signature.
- `fixture.pkg` is an unsigned component package made with `pkgbuild`.
- `SignedFixture.app` and `signed-fixture.pkg` use Woodleigh School's Developer ID
  Application and Installer identities, team `SMLKBTR495`.

The signed fixtures test cryptographic verification independently of a machine's
keychain. Tests use Apple tools as additional oracles when available.
