# Changelog

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
