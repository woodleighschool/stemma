# Changelog

## [0.3.0](https://github.com/woodleighschool/stemma/compare/0.2.1...0.3.0) (2026-09-15)


### ⚠ BREAKING CHANGES

* verify publisher signatures with one signature policy

### Features

* derive and refresh MacSoftware icons automatically ([d7ca830](https://github.com/woodleighschool/stemma/commit/d7ca830f455be35fec185b6b3fbc3c1293155393))
* derive Munki pkginfo defaults like makepkginfo ([51175eb](https://github.com/woodleighschool/stemma/commit/51175eb32cbf4a5189e43501b37f2cedc13eefcf))
* derive signers with stemma signature ([0c7168d](https://github.com/woodleighschool/stemma/commit/0c7168dff08f164bb8d6475fbf59a870a4dea176))
* **npm:** update dependency oxlint (1.82.0 → 1.83.0) ([#21](https://github.com/woodleighschool/stemma/issues/21)) ([922da39](https://github.com/woodleighschool/stemma/commit/922da39f4bef5a8c549a7779004407a3e2838617))
* record static PKG installer declarations ([4b755f7](https://github.com/woodleighschool/stemma/commit/4b755f798cb40038b7edd7f42b1394116f9d96a8))
* verify publisher signatures with one signature policy ([145554a](https://github.com/woodleighschool/stemma/commit/145554a2f1382fb7c8e53849a3e28d2b44f1d737))


### Bug Fixes

* bubbletea bug ([aa4b726](https://github.com/woodleighschool/stemma/commit/aa4b726af1895010ef4e2c37082a9d2a8d9d6af1))
* resolve GitHub assets with unambiguous globs ([ca0cf6e](https://github.com/woodleighschool/stemma/commit/ca0cf6e0c3d396cfb53681c6f15e5555e0c4a1e1))


### Performance Improvements

* read DMGs lazily and streamline app verification ([dbf714c](https://github.com/woodleighschool/stemma/commit/dbf714c46ad4be30f7f562ee03713d404a7bd5ee))
* stream verified PKG payloads without a pipe ([323998a](https://github.com/woodleighschool/stemma/commit/323998a387984e4af8f9311550773f8064858798))
* use concurrent package readers from fork ([16800d4](https://github.com/woodleighschool/stemma/commit/16800d4c93f3e5fc1f19f001c2e1e3432ea84865))
* verify apps without hashing the whole bundle ([bc63065](https://github.com/woodleighschool/stemma/commit/bc6306599540ddbcf0968114646accb452d68372))


### Miscellaneous Chores

* **deps:** update xo/terminfo ([9cf6684](https://github.com/woodleighschool/stemma/commit/9cf668478ae0986b30c20074ac9c2332d53b3d6b))
* fix temp lint exclusions ([cfc045a](https://github.com/woodleighschool/stemma/commit/cfc045ac35f016bc63cd0b45be0e934a1441a046))

## [0.2.1](https://github.com/woodleighschool/stemma/compare/0.2.0...0.2.1) (2026-09-13)


### Bug Fixes

* bump mise deps, refresh lockfile ([92112f1](https://github.com/woodleighschool/stemma/commit/92112f1ce3abb7ccfdc3ac4a0cd61cf87c0c0f95))
* remove overflowing map allocation hints ([4a9d5af](https://github.com/woodleighschool/stemma/commit/4a9d5af9130389137feb577ef33972614bce5cdc))


### Miscellaneous Chores

* add ignore for node_modules in go.mod ([8233f49](https://github.com/woodleighschool/stemma/commit/8233f49a0dfec8071c67cf39c4fc5df072647c68))
* readme tweaks ([a266a82](https://github.com/woodleighschool/stemma/commit/a266a8209842d989e24b1b2530a20cea6888f741))

## [0.2.0](https://github.com/woodleighschool/stemma/compare/0.1.0...0.2.0) (2026-09-13)


### ⚠ BREAKING CHANGES

* separate builds from native software publication
* reconcile native software destinations
* support software catalogs and portable vendor artifacts
* distribute plugin bundles through OCI
* expose operations through the plugin protocol
* expand environment variables in configuration

### Features

* distribute plugin bundles through OCI ([9f07903](https://github.com/woodleighschool/stemma/commit/9f0790309c6e166bc0dac6ef11bcce101ac2e923))
* expand environment variables in configuration ([5604cb2](https://github.com/woodleighschool/stemma/commit/5604cb2dca1386e777e959f124a9336c893a8562))
* expose operations through the plugin protocol ([87cbf91](https://github.com/woodleighschool/stemma/commit/87cbf9153f0698670c6d52e3dc71f72cb91620bf))
* infer source filenames and name installer outputs ([ad32801](https://github.com/woodleighschool/stemma/commit/ad32801be9d4608f06434a287d02167f614a9aa8))
* load local plugins and prepare input locks ([e7e4d73](https://github.com/woodleighschool/stemma/commit/e7e4d73b0c2aa477c4076da3b1097c218dda80e7))
* reconcile native software destinations ([a4ab61f](https://github.com/woodleighschool/stemma/commit/a4ab61f8d1f7ca703d894201aa09ed51dd545f05))
* separate builds from native software publication ([092c906](https://github.com/woodleighschool/stemma/commit/092c906807cd6625fd4aeb49dd9326bb5baac576))
* support software catalogs and portable vendor artifacts ([9130bf7](https://github.com/woodleighschool/stemma/commit/9130bf78ea31bb73b7dced87f6c48ac282c7c21e))
* unify command progress and diagnostics ([4aee345](https://github.com/woodleighschool/stemma/commit/4aee345c40e4ab85c29885588f74a20900d67802))


### Bug Fixes

* normalize macOS source storage metadata ([c841201](https://github.com/woodleighschool/stemma/commit/c84120166ae7f245cf47219aed8a94701ca94162))
* prepare common vendor Mac installers ([47255c0](https://github.com/woodleighschool/stemma/commit/47255c08c0e98f24b52f998ef9ac6855ce0b3311))
* render grouped terminal progress ([beeb0dd](https://github.com/woodleighschool/stemma/commit/beeb0ddec952935dbe97a518bf7dbf36364df80d))


### Code Refactoring

* keep host architecture policy in destinations ([9fc2d57](https://github.com/woodleighschool/stemma/commit/9fc2d57cc3aecb9db2d11d73cbe872045e6fc794))
* scope Intune clients and simplify native icon rendering ([4bc469f](https://github.com/woodleighschool/stemma/commit/4bc469f7acafcb1c7793acb2cc4b99ce7b4c8a1d))
* tighten destination plugin boundaries ([0f10fce](https://github.com/woodleighschool/stemma/commit/0f10fcef286158abe1553ce4a73186d5bfac5a84))
* use package format primitives ([359e6db](https://github.com/woodleighschool/stemma/commit/359e6db61dbe3a5d1a5683d5fc76abe6dd009249))


### Documentation

* add Docusaurus docs ([273025a](https://github.com/woodleighschool/stemma/commit/273025aa07bb3d949bd2fe909c24fca6e2c84850))


### Miscellaneous Chores

* **deps:** update Go dependencies ([211b5cc](https://github.com/woodleighschool/stemma/commit/211b5ccf28841f78a763ceff7d8257bbdfc9ec8b))
* **mise:** update mise tools ([#11](https://github.com/woodleighschool/stemma/issues/11)) ([9eb7042](https://github.com/woodleighschool/stemma/commit/9eb7042482d7b00c872c149eea4a885f9a838ab5))
* **npm:** update dependency pnpm (12.3.4 → 12.4.1) ([#16](https://github.com/woodleighschool/stemma/issues/16)) ([823ef10](https://github.com/woodleighschool/stemma/commit/823ef109aac0c69bdc1ab9f52468b362e369e75d))
* remove redundant cpio dependency ([6f98459](https://github.com/woodleighschool/stemma/commit/6f98459c10a8771daa1b78e3d5d38298938107b9))
* strengthen Go linting and resolve findings ([53a4a02](https://github.com/woodleighschool/stemma/commit/53a4a02c60318b394f1d6c69acfd00b672de9da9))

## 0.1.0 (2026-09-06)


### Features

* adopt deployment SDKs and simplify local setup ([14778b5](https://github.com/woodleighschool/stemma/commit/14778b578e7393d6780a964f3f5ecd6b07d202c0))
* bootstrap stemma ([50ec7ea](https://github.com/woodleighschool/stemma/commit/50ec7ead044cf029cb5f901471acadc60949305d))
* publish configuration schema ([906440a](https://github.com/woodleighschool/stemma/commit/906440a67981243e9d85d826ad14e021ec565ce3))


### Tests

* narrow native CI and simplify fixtures ([64a2621](https://github.com/woodleighschool/stemma/commit/64a2621ee6a41d2c0960b655caef6b71a82750ea))


### Continuous Integration

* validate release archives and skip metadata checks ([95e706c](https://github.com/woodleighschool/stemma/commit/95e706c9479a0ffe0a21018346caee6e5cd0d30c))


### Miscellaneous Chores

* replace workflow lint task with local action hook ([d19dd0c](https://github.com/woodleighschool/stemma/commit/d19dd0cb0d8220c129d41ecfafa8e89fc8f2691f))
