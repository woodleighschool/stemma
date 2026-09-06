# Apple fixtures

Rebuild from this directory on macOS with Xcode's command-line tools and the
Developer ID identities below. The apps cover ad-hoc and CMS signatures; the PKG
covers installer signature verification.

```sh
work=$(mktemp -d)
printf 'int main(void) { return 0; }\n' > "$work/main.c"
clang -arch arm64 -arch x86_64 -mmacosx-version-min=13.0 \
  -o "$work/fixture" "$work/main.c"
cp "$work/fixture" Fixture.app/Contents/MacOS/fixture
codesign --force --sign - --timestamp=none Fixture.app
cp "$work/fixture" SignedFixture.app/Contents/MacOS/fixture
codesign --force --timestamp=none \
  --sign 'Developer ID Application: Woodleigh School (SMLKBTR495)' SignedFixture.app
pkgbuild --component SignedFixture.app --install-location /Applications \
  --identifier au.edu.vic.woodleigh.stemma.fixture --version 1.2.3 "$work/unsigned.pkg"
productsign --timestamp=none \
  --sign 'Developer ID Installer: Woodleigh School (SMLKBTR495)' \
  "$work/unsigned.pkg" fixture.pkg
rm -rf "$work"
```

If the installer certificate changes, update its SHA-256 pin in `apple_test.go`.
