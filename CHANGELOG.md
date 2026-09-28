# Changelog

## [0.7.1](https://github.com/woodleighschool/stemma/compare/v0.7.0...v0.7.1) (2026-09-28)


### Bug Fixes

* **intune:** report minimum OS as a single version ([a49e9c3](https://github.com/woodleighschool/stemma/commit/a49e9c3b0184ecc097a4b76e2abf38708ee5c105))

## [0.7.0](https://github.com/woodleighschool/stemma/compare/0.6.0...v0.7.0) (2026-09-28)


### ⚠ BREAKING CHANGES

* **intune:** remove unsupported content retention

### Features

* **intune:** remove unsupported content retention ([cbf8e19](https://github.com/woodleighschool/stemma/commit/cbf8e19f5f9d030714aad0d0290fe73ffdf9a8f1))


### Bug Fixes

* **intune:** resume content publication ([8cae52d](https://github.com/woodleighschool/stemma/commit/8cae52d3fd4b1c52cc073e14de34a6bad5592609))
* **munki:** truncate installer sizes to whole KiB ([0de624a](https://github.com/woodleighschool/stemma/commit/0de624aa2763b2073da958c9a1887094fe67eed9))


### Continuous Integration

* prefix release tags for Go module consumers ([6f986a8](https://github.com/woodleighschool/stemma/commit/6f986a89e0ec97f298d7cbe416bb8e4feceb066c))
* skip checks for generated release commits ([e4466b0](https://github.com/woodleighschool/stemma/commit/e4466b041c0fa04ad863694495cb5ed1222eed6e))

## [0.6.0](https://github.com/woodleighschool/stemma/compare/0.5.0...0.6.0) (2026-09-28)


### ⚠ BREAKING CHANGES

* compare preparation across plugin versions

### Features

* compare preparation across plugin versions ([462ae74](https://github.com/woodleighschool/stemma/commit/462ae740f942f0b20033a4dca31f4baff9f31099))
* declare disk image and package compression ([cdd58b7](https://github.com/woodleighschool/stemma/commit/cdd58b7beca3650b53bb78de968f192f45b53bbd))
* **go:** update module github.com/deploymenttheory/go-apfs-v2 (v0.11.3 → v0.13.0) ([#57](https://github.com/woodleighschool/stemma/issues/57)) ([a447d59](https://github.com/woodleighschool/stemma/commit/a447d59b2adb9316fd8a88e6a3a2466663b9e05c))


### Bug Fixes

* allow absent bundle executable declarations ([08f7fed](https://github.com/woodleighschool/stemma/commit/08f7fed32515a7bb5147fe73c23941a2d7f267e1))
* **go:** update module github.com/deploymenttheory/go-apfs-v2 (v0.11.2 → v0.11.3) ([#31](https://github.com/woodleighschool/stemma/issues/31)) ([a9934e5](https://github.com/woodleighschool/stemma/commit/a9934e51b095c2063a513886057ca1af8a3bd006))
* **go:** update module github.com/deploymenttheory/go-bindings-macosplatform (v0.20.1-0.20260927034632-71544a14f48a → v0.20.1) ([#56](https://github.com/woodleighschool/stemma/issues/56)) ([66a52ad](https://github.com/woodleighschool/stemma/commit/66a52ad80f75ec60a565665741beaef7ad703126))


### Performance Improvements

* **apple:** decode PBZX payloads with up to eight workers ([a387cff](https://github.com/woodleighschool/stemma/commit/a387cffe7b761c9e76203bbe8dad53be4a6c9ab0))
* **pkgbuild:** compress payloads with klauspost gzip ([ab7694b](https://github.com/woodleighschool/stemma/commit/ab7694b0c8aba045a61bce4e358aead6d2c0c9f6))


### Documentation

* reorganize guides and refresh catalog documentation ([225888a](https://github.com/woodleighschool/stemma/commit/225888a30e3012d5d2f4e7469c6679f95a99329c))


### Miscellaneous Chores

* align formatter ignores and rebuild tool locks ([d11def5](https://github.com/woodleighschool/stemma/commit/d11def59e5de3a672b7702fe494a61ca7547f74f))
* include dependency licenses in release artifacts ([d6b6c84](https://github.com/woodleighschool/stemma/commit/d6b6c84ec3bc887af12e56c92938f4db63dd8fd8))
* remove release version bootstrap overrides ([927385a](https://github.com/woodleighschool/stemma/commit/927385a5e4b09d6673e7dba023fd5d3965d19922))
* tidy modules directly in GoReleaser ([77e4ce7](https://github.com/woodleighschool/stemma/commit/77e4ce745672c5aa42e66c69d3b3a07f6433ad88))

## [0.5.0](https://github.com/woodleighschool/stemma/compare/0.4.0...0.5.0) (2026-09-27)


### ⚠ BREAKING CHANGES

* declare signing expectations per subject

### Features

* declare signing expectations per subject ([b3ba758](https://github.com/woodleighschool/stemma/commit/b3ba758fdf00bb0e59df200eb72ad380238f240e))


### Build System

* **deps:** use upstream macOS platform bindings ([0e2798f](https://github.com/woodleighschool/stemma/commit/0e2798fab42562a3014c536bb9854bef61f83266))

## [0.4.0](https://github.com/woodleighschool/stemma/compare/0.3.0...0.4.0) (2026-09-27)


### ⚠ BREAKING CHANGES

* **plugin:** version operations and publish portable bundles
* record input locks in update and compare catalog changes
* evaluate settings only where commands use them
* **windows:** prepare setup directories and derive Intune apps
* **cli:** stream preparation progress and artifact reports
* **source:** separate fetch state from reviewed inputs

### Features

* **cli:** stream preparation progress and artifact reports ([d6ddf1c](https://github.com/woodleighschool/stemma/commit/d6ddf1c97608f6e5d542dc186985e7729a268d90))
* evaluate settings only where commands use them ([412ba59](https://github.com/woodleighschool/stemma/commit/412ba596ccaca681468c65184bf7bf137cfbad1f))
* **go:** update module github.com/deploymenttheory/go-apfs-v2 (v0.10.0 → v0.11.2) ([#50](https://github.com/woodleighschool/stemma/issues/50)) ([4aaff3b](https://github.com/woodleighschool/stemma/commit/4aaff3badfe5313d544391828e1b973a2e310e9e))
* **macpkg:** verify wrapped installer signatures ([3f5f158](https://github.com/woodleighschool/stemma/commit/3f5f1583089f2463801e578669f535965b6e1a46))
* **mcp:** expose catalog discovery and preparation tools ([f029a47](https://github.com/woodleighschool/stemma/commit/f029a477be924802ff5688963d2d9f4f5575a4d0))
* **plugin:** version operations and publish portable bundles ([a833986](https://github.com/woodleighschool/stemma/commit/a833986f109d411ff350bf3be3b98ceab2705b97))
* record input locks in update and compare catalog changes ([1a8db25](https://github.com/woodleighschool/stemma/commit/1a8db254d1bac9f1ab7c95d33361fc29d5c4a6f5))
* **source:** separate fetch state from reviewed inputs ([40709bc](https://github.com/woodleighschool/stemma/commit/40709bcba9ceaeb6156a3dbd60542675f4839786))
* **windows:** prepare setup directories and derive Intune apps ([58567bf](https://github.com/woodleighschool/stemma/commit/58567bfbeb8a8f3e8bd37cc2e2cbc2a8bba94985))


### Bug Fixes

* **mac:** inspect applets and render native icons ([cd34f7a](https://github.com/woodleighschool/stemma/commit/cd34f7a1e9b7fabfb0b18916b84fb9948c4f8765))


### Miscellaneous Chores

* **deps:** drop the lzfse fork replace ([dc41e5c](https://github.com/woodleighschool/stemma/commit/dc41e5cbe49dcbebe5e123c8677b7d76bd8ecbf8))

## [0.3.0](https://github.com/woodleighschool/stemma/compare/0.2.1...0.3.0) (2026-09-26)


### ⚠ BREAKING CHANGES

* **source:** parse download links from HTML attributes
* **jamf:** resolve deployment objects by name
* **intune:** use semantic metadata and support Mac LOB apps
* derive destination metadata from prepared software
* derive plugin contracts from typed configs
* use canonical publication references
* reconcile destinations without local state
* flatten source resolver inputs
* preparation no longer produces icons and `--refresh-icons`, the `refresh_icons` request field and resource `cache_variants` are removed. Declare `spec.icon` and commit the file `stemma icon` renders.
* verify publisher signatures with one signature policy

### Features

* **actions:** add publish-plugin ([dd61b5a](https://github.com/woodleighschool/stemma/commit/dd61b5aaac7a9d97f6eb9b3f0848ae67818a4ffb))
* add stemma reconcile ([4134135](https://github.com/woodleighschool/stemma/commit/41341355c91626178109d85a2f4bd2ee3862617d))
* **apple:** extract an application bundle from a package payload ([c9f9ef6](https://github.com/woodleighschool/stemma/commit/c9f9ef6abaed8792a686f78891ee6480d004cc91))
* **cli:** print text reports with optional JSON ([1d7f04e](https://github.com/woodleighschool/stemma/commit/1d7f04e08860123f96b7188a19ad42c0deb146f6))
* compose package contents and evaluate typed expressions ([42bfb71](https://github.com/woodleighschool/stemma/commit/42bfb7145b70156520e7627768094878d265c399))
* derive and refresh MacSoftware icons automatically ([d7ca830](https://github.com/woodleighschool/stemma/commit/d7ca830f455be35fec185b6b3fbc3c1293155393))
* derive destination metadata from prepared software ([80ca318](https://github.com/woodleighschool/stemma/commit/80ca318417ac1d759352f4f72a3d99a683a5e605))
* derive Munki pkginfo defaults like makepkginfo ([51175eb](https://github.com/woodleighschool/stemma/commit/51175eb32cbf4a5189e43501b37f2cedc13eefcf))
* derive signers with stemma signature ([0c7168d](https://github.com/woodleighschool/stemma/commit/0c7168dff08f164bb8d6475fbf59a870a4dea176))
* **engine:** expose candidate resolution and lock helpers ([9121a3d](https://github.com/woodleighschool/stemma/commit/9121a3dad234c4bbdb8e228f1444db7080c83c34))
* **engine:** leave software outside the run to its destination ([5be1d3d](https://github.com/woodleighschool/stemma/commit/5be1d3d4a980e3bca2f0cfe89feb93d159010c87))
* inspect bundles and extract icons across platforms ([22301b9](https://github.com/woodleighschool/stemma/commit/22301b9a6bb343708583dc304c88d446e6c16499))
* **intune:** use semantic metadata and support Mac LOB apps ([88fc704](https://github.com/woodleighschool/stemma/commit/88fc704413d56788a3837c4a5aeff786a49ca4e7))
* **jamf:** resolve deployment objects by name ([d7da826](https://github.com/woodleighschool/stemma/commit/d7da8262468b812e27256801bdaeb250c90d99f9))
* **macpkg:** expose input facts to package expressions ([cbc1483](https://github.com/woodleighschool/stemma/commit/cbc148347529178395f049686d2ec0ba54f8ddcc))
* **macsoftware:** build disk images from application archives ([3929992](https://github.com/woodleighschool/stemma/commit/39299927012073852560ad069dd620a9e2f078f3))
* **macsoftware:** inspect and verify installer inventories ([3505b97](https://github.com/woodleighschool/stemma/commit/3505b97548b110d537d83f29420bbbbb12f7be4b))
* **npm:** update dependency oxlint (1.82.0 → 1.83.0) ([#21](https://github.com/woodleighschool/stemma/issues/21)) ([922da39](https://github.com/woodleighschool/stemma/commit/922da39f4bef5a8c549a7779004407a3e2838617))
* **npm:** update dependency oxlint (1.83.0 → 1.84.0) ([#35](https://github.com/woodleighschool/stemma/issues/35)) ([ae1c59a](https://github.com/woodleighschool/stemma/commit/ae1c59a8c5cee9c91d8a8fd3870528ede513cbf5))
* **npm:** update dependency oxlint (1.84.0 → 1.85.0) ([#36](https://github.com/woodleighschool/stemma/issues/36)) ([109b815](https://github.com/woodleighschool/stemma/commit/109b81546059b4f2185c4f04b03dcd16f8dfd86f))
* **plugins:** publish GoReleaser releases as OCI plugins ([9aa8d17](https://github.com/woodleighschool/stemma/commit/9aa8d1783e47ba62f8f67edf51c585b983f42b0c))
* publish committed icons and render them with stemma icon ([28f32ef](https://github.com/woodleighschool/stemma/commit/28f32efe11f19bef2572b855acf5e3a83996655f))
* **reconcile:** confirm pending proposals without downloading them again ([4570b3d](https://github.com/woodleighschool/stemma/commit/4570b3d08c91d915bcf1393ae2f97bf620854700))
* **reconcile:** leave suspended resources out of proposals ([2f47d64](https://github.com/woodleighschool/stemma/commit/2f47d646c23eb87f77272591aede20e133e6150e))
* **reconcile:** reconcile Git-backed projects through configured source control ([934f0ef](https://github.com/woodleighschool/stemma/commit/934f0efc6962f492a26450e5e03c77b57ad4ac8c))
* record static PKG installer declarations ([4b755f7](https://github.com/woodleighschool/stemma/commit/4b755f798cb40038b7edd7f42b1394116f9d96a8))
* **source:** accept absolute file input paths ([b86b3f8](https://github.com/woodleighschool/stemma/commit/b86b3f8981fdf61e3fd0de9a74ff81537e2bb5a4))
* **source:** include GitHub prereleases ([1f620ff](https://github.com/woodleighschool/stemma/commit/1f620ff7a7e0604910f3358cd1a6ec0fab099a1d))
* **source:** parse download links from HTML attributes ([5a8ee0e](https://github.com/woodleighschool/stemma/commit/5a8ee0efb7ffb5606265399c89bb50074c3f6cbd))
* **source:** refresh remote inputs efficiently ([ba2a0e6](https://github.com/woodleighschool/stemma/commit/ba2a0e68f8099864d7ecff7a6f58f3eb4602091e))
* **source:** resolve relative HTTP download links ([ba6b62f](https://github.com/woodleighschool/stemma/commit/ba6b62fad941bcf43b2073974a9a0510722bce09))
* suspend resources from runs without selectors ([929123d](https://github.com/woodleighschool/stemma/commit/929123d12cac223970f200f4a771bf929eca86cc))
* verify publisher signatures with one signature policy ([145554a](https://github.com/woodleighschool/stemma/commit/145554a2f1382fb7c8e53849a3e28d2b44f1d737))


### Bug Fixes

* **apple:** report cancellation when payload reads are interrupted ([61952bd](https://github.com/woodleighschool/stemma/commit/61952bdc04fbad6c54b1c0581f76025d1dd66d0e))
* **archive:** carry POSIX names through trees and images ([bc27771](https://github.com/woodleighschool/stemma/commit/bc27771f8f337862081093df4ec72a401f0d4ad8))
* **archive:** normalize symlink permissions ([677f00f](https://github.com/woodleighschool/stemma/commit/677f00fae1fa9db0539c6f7094469add56ef1c17))
* bubbletea bug ([aa4b726](https://github.com/woodleighschool/stemma/commit/aa4b726af1895010ef4e2c37082a9d2a8d9d6af1))
* **deps:** pin lzfse to a fork that reads short literal payloads ([b9d878a](https://github.com/woodleighschool/stemma/commit/b9d878a12241c5fe5a50e78d13c8bc0610f0d394))
* **deps:** pin package signature verification ([021a6bc](https://github.com/woodleighschool/stemma/commit/021a6bca215518691d887ad4989e712c4661cc49))
* **deps:** read compressed disk image chunks once ([8b69848](https://github.com/woodleighschool/stemma/commit/8b698487b097a9f11db5209269a6272d329830b6))
* **deps:** update go-apfs-v2 to v0.10.0 ([a490c2c](https://github.com/woodleighschool/stemma/commit/a490c2c44ffdd513e943abcc1180ac0b97265cbd))
* **deps:** use released go-macos-pkg v0.7.1 ([00a6484](https://github.com/woodleighschool/stemma/commit/00a6484bebac12f79b875280225547234db4e420))
* **diskimage:** separate logical capacity from read limits ([f8f58c2](https://github.com/woodleighschool/stemma/commit/f8f58c2c3079750cb191bf814760fd678f8d4d12))
* **engine:** isolate resource failures ([3d76b46](https://github.com/woodleighschool/stemma/commit/3d76b46f15ce560774b1dad072dd6f4286c637a7))
* **engine:** scope resource evaluation to the selected closure ([bec58c2](https://github.com/woodleighschool/stemma/commit/bec58c21973c9cac344aab6439afeacc963c1879))
* **go:** update module charm.land/bubbletea/v2 (v2.0.9 → v2.0.10) ([#41](https://github.com/woodleighschool/stemma/issues/41)) ([69b7ba7](https://github.com/woodleighschool/stemma/commit/69b7ba7a6ef976ae50c50ba72b29ffd38b304684))
* **go:** update module github.com/bmatcuk/doublestar/v4 (v4.10.0 → v4.10.2) ([#30](https://github.com/woodleighschool/stemma/issues/30)) ([ed50dfd](https://github.com/woodleighschool/stemma/commit/ed50dfd90408f3088147a0c626a37d109fee1c49))
* **go:** update module github.com/deploymenttheory/go-macos-pkg (v0.7.2-0.20260924122851-a012bc692bc4 → v0.7.2) ([#44](https://github.com/woodleighschool/stemma/issues/44)) ([0245c3e](https://github.com/woodleighschool/stemma/commit/0245c3ed59a304c08255b1628e0d5f171a2793e3))
* **go:** update module github.com/ebitengine/purego (v0.11.0 → v0.11.1) ([#34](https://github.com/woodleighschool/stemma/issues/34)) ([c68f72c](https://github.com/woodleighschool/stemma/commit/c68f72ca01d922a51ce27516dca373b89e08b04c))
* **go:** update module github.com/klauspost/compress (v1.20.0 → v1.20.1) ([#46](https://github.com/woodleighschool/stemma/issues/46)) ([81a8b66](https://github.com/woodleighschool/stemma/commit/81a8b663ebb28c32466d42d086f2c2224352a78f))
* **go:** update module github.com/microsoft/kiota-abstractions-go (v1.11.0 → v1.11.1) ([#24](https://github.com/woodleighschool/stemma/issues/24)) ([0901c4a](https://github.com/woodleighschool/stemma/commit/0901c4ac2d355852ae429ab17453742fb51ef059))
* keep resource work in one progress tree ([a8e870b](https://github.com/woodleighschool/stemma/commit/a8e870b4fee561af9874379a3efe55ff0b85da53))
* **macsoftware:** allow packages with multiple applications ([308b6cc](https://github.com/woodleighschool/stemma/commit/308b6cc02f2a0ef092e966822f23c8051fb1757b))
* **plugin:** keep integer and duration types in forwarded logs ([388637f](https://github.com/woodleighschool/stemma/commit/388637f436f79e47b2ff26cb3aedce79b1a3c5af))
* preserve icon artwork with minimal bundle extraction ([5767907](https://github.com/woodleighschool/stemma/commit/5767907aedfac5799255e62e7dc0f091faba9c21))
* preserve resolver evidence through source locks ([9491f0a](https://github.com/woodleighschool/stemma/commit/9491f0ab41fdd8740c3e1057c2a3d9d1d98df4e8))
* resolve GitHub assets with unambiguous globs ([ca0cf6e](https://github.com/woodleighschool/stemma/commit/ca0cf6e0c3d396cfb53681c6f15e5555e0c4a1e1))
* respect Windows filesystem semantics ([92d1cee](https://github.com/woodleighschool/stemma/commit/92d1cee3daa4badd68a1154f4148e3fab568081f))
* show nested stages on their operation row ([bbd7182](https://github.com/woodleighschool/stemma/commit/bbd71824457ffefbc3fc0b2ef5558cd9c852b569))


### Performance Improvements

* read DMGs lazily and streamline app verification ([dbf714c](https://github.com/woodleighschool/stemma/commit/dbf714c46ad4be30f7f562ee03713d404a7bd5ee))
* stream verified PKG payloads without a pipe ([323998a](https://github.com/woodleighschool/stemma/commit/323998a387984e4af8f9311550773f8064858798))
* use concurrent package readers from fork ([16800d4](https://github.com/woodleighschool/stemma/commit/16800d4c93f3e5fc1f19f001c2e1e3432ea84865))
* verify apps without hashing the whole bundle ([bc63065](https://github.com/woodleighschool/stemma/commit/bc6306599540ddbcf0968114646accb452d68372))


### Code Refactoring

* derive plugin contracts from typed configs ([19204a5](https://github.com/woodleighschool/stemma/commit/19204a5000b92a982cf62b37eb0614ed55e14987))
* finish stages on their progress row ([8aa4ce6](https://github.com/woodleighschool/stemma/commit/8aa4ce631c2e562b7759ccea7af9a390e03181d4))
* flatten source resolver inputs ([fa762be](https://github.com/woodleighschool/stemma/commit/fa762be056fa7d983f099fedff63d1a1d3d0f504))
* keep packaging inputs portable ([aa2cc7a](https://github.com/woodleighschool/stemma/commit/aa2cc7acf74243c594a01bed96723e96171d5166))
* **plugins:** move to oras-go v3 ([7986ab4](https://github.com/woodleighschool/stemma/commit/7986ab4920514a9693791d1af2546f513b9cdc4e))
* reconcile destinations without local state ([5c2cc6a](https://github.com/woodleighschool/stemma/commit/5c2cc6a2d25ff97c6b75fb4ee9efb0bc52f4a749))
* share package and project test fixtures ([2adbaf6](https://github.com/woodleighschool/stemma/commit/2adbaf6111c5a1a4a55606f4e7b63c4622064814))
* use canonical publication references ([71ee3ca](https://github.com/woodleighschool/stemma/commit/71ee3ca0da1dc6a4421d0fd94a1aef8d3b5efe02))


### Tests

* use a Windows-valid icon filename ([2b7c1cf](https://github.com/woodleighschool/stemma/commit/2b7c1cf87f061ade2ea220f6ccfdb1b9ef4cf0fd))


### Build System

* drop the setup action in favour of Mise-pinned releases ([7739d48](https://github.com/woodleighschool/stemma/commit/7739d489f709ced5b26d28d453f37cb338ded2ac))
* publish the container through the release pipeline ([0f70cf1](https://github.com/woodleighschool/stemma/commit/0f70cf1adadbe1d1a09b119ce4c9fb6552b1c703))


### Continuous Integration

* test every supported release platform ([b581b29](https://github.com/woodleighschool/stemma/commit/b581b29fc2fe1a3efe2081753788c8556a1bffd6))


### Miscellaneous Chores

* **deps:** update xo/terminfo ([9cf6684](https://github.com/woodleighschool/stemma/commit/9cf668478ae0986b30c20074ac9c2332d53b3d6b))
* fix temp lint exclusions ([cfc045a](https://github.com/woodleighschool/stemma/commit/cfc045ac35f016bc63cd0b45be0e934a1441a046))
* **github-action:** update action ubuntu (24.04 → 26.04) ([#26](https://github.com/woodleighschool/stemma/issues/26)) ([84910f9](https://github.com/woodleighschool/stemma/commit/84910f917ed29a4b9b62277b200c7afb42106b5a))
* **github-action:** Update github-actions ([#29](https://github.com/woodleighschool/stemma/issues/29)) ([d489221](https://github.com/woodleighschool/stemma/commit/d489221b3e3e67f5c8fbd68c716de2589bb11f6e))
* **lint:** use nolint directives instead of exclusion rules ([93ce641](https://github.com/woodleighschool/stemma/commit/93ce64128e8ba08b96282c25315033da862e5460))
* **mise:** update mise tools ([#23](https://github.com/woodleighschool/stemma/issues/23)) ([50c5c18](https://github.com/woodleighschool/stemma/commit/50c5c1883bcf83be1217761afb3ef68eaf5c4869))
* **mise:** update tool node (26.8.2 → v26.9.0) ([#14](https://github.com/woodleighschool/stemma/issues/14)) ([03c924e](https://github.com/woodleighschool/stemma/commit/03c924efecb7b40ffac323321dbb86af3fd4576a))
* **mise:** update tool node (26.9.0 → v26.10.0) ([#40](https://github.com/woodleighschool/stemma/issues/40)) ([6e46bfd](https://github.com/woodleighschool/stemma/commit/6e46bfdc3eb39e1cee02ab1b5203f6d5e5232d83))
* **mise:** update tool npm:@commitlint/cli (21.2.2 → 21.2.3) ([#37](https://github.com/woodleighschool/stemma/issues/37)) ([302a1c2](https://github.com/woodleighschool/stemma/commit/302a1c22ba9742dad3cf712738e8297984faf8b6))
* **mise:** update tool oxfmt (0.68.0 → 0.69.0) ([#42](https://github.com/woodleighschool/stemma/issues/42)) ([6d7ec32](https://github.com/woodleighschool/stemma/commit/6d7ec32f21a36534c06f23fc318fb891b114c765))
* **mise:** update tool oxfmt (0.69.0 → 0.70.0) ([#43](https://github.com/woodleighschool/stemma/issues/43)) ([b3749a1](https://github.com/woodleighschool/stemma/commit/b3749a11a21afcf97047f081f038f9028b804490))
* **npm:** update dependency pnpm (12.4.1 → 12.5.1) ([#22](https://github.com/woodleighschool/stemma/issues/22)) ([52ed43b](https://github.com/woodleighschool/stemma/commit/52ed43bd93961205bc8dea3d7b68b8a878ac1978))
* **npm:** update dependency pnpm (12.5.1 → 12.6.0) ([#39](https://github.com/woodleighschool/stemma/issues/39)) ([b5dfc76](https://github.com/woodleighschool/stemma/commit/b5dfc76650613e157aa9300254960be454bbf8e4))

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
