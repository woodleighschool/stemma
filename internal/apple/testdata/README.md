# Apple fixtures

Rebuild from this directory on macOS with Xcode's command-line tools and the
Developer ID identities below. The apps cover ad-hoc, Developer ID and nested
code signatures; the PKG covers installer signature verification. Trusted
timestamps keep the signed fixtures valid after the certificates expire.

```sh
work=$(mktemp -d)
identity='Developer ID Application: Woodleigh School (SMLKBTR495)'
printf 'int main(void) { return 0; }\n' > "$work/main.c"
printf 'int nested_value(void) { return 42; }\n' > "$work/lib.c"
build() { clang -arch arm64 -arch x86_64 -mmacosx-version-min=13.0 "$@"; }
build -o "$work/fixture" "$work/main.c"
cp "$work/fixture" Fixture.app/Contents/MacOS/fixture
codesign --force --sign - --timestamp=none Fixture.app
cp "$work/fixture" SignedFixture.app/Contents/MacOS/fixture
codesign --force --timestamp --sign "$identity" SignedFixture.app
pkgbuild --component SignedFixture.app --install-location /Applications \
  --identifier au.edu.vic.woodleigh.stemma.fixture --version 1.2.3 "$work/unsigned.pkg"
productsign --timestamp \
  --sign 'Developer ID Installer: Woodleigh School (SMLKBTR495)' \
  "$work/unsigned.pkg" fixture.pkg

nested=NestedFixture.app/Contents
cp "$work/fixture" "$nested/MacOS/fixture"
build -sectcreate __TEXT __info_plist "$work/helper.plist" -o "$nested/MacOS/helper" "$work/main.c"
build -dynamiclib -install_name @rpath/Nested.framework/Versions/A/Nested \
  -o "$nested/Frameworks/Nested.framework/Versions/A/Nested" "$work/lib.c"
build -dynamiclib -install_name @rpath/Shallow.framework/Shallow \
  -o "$nested/Frameworks/Shallow.framework/Shallow" "$work/lib.c"
cp "$work/fixture" "$nested/Helpers/Helper.app/Contents/MacOS/Helper"
codesign --force --deep --timestamp --sign "$identity" NestedFixture.app
rm -rf "$work"
```

`helper.plist` is a minimal Info.plist naming `au.edu.vic.woodleigh.stemma.nested.helper`.
NestedFixture.app keeps its Info.plist files, `PkgInfo`, resources, symlinks and
framework layout in the tree; only the binaries and signatures are rebuilt.

RequirementFixture.app records the Developer ID requirement for its `helper` and
a weaker one for `weak`. `replacements/` holds that helper re-signed with a new
cdhash, under another identifier and ad hoc. Only the Info.plist stays in the tree.

```sh
work=$(mktemp -d)
identity='Developer ID Application: Woodleigh School (SMLKBTR495)'
helper=au.edu.vic.woodleigh.stemma.requirement.helper
printf 'int main(void) { return 0; }\n' > "$work/main.c"
clang -arch arm64 -mmacosx-version-min=13.0 -o "$work/fixture" "$work/main.c"
app=RequirementFixture.app/Contents
mkdir -p "$app/MacOS" "$app/Helpers" replacements
for name in MacOS/fixture Helpers/helper Helpers/weak; do cp "$work/fixture" "$app/$name"; done
codesign --force --timestamp --sign "$identity" -i "$helper" "$app/Helpers/helper"
codesign --force --timestamp --sign "$identity" -i "$helper" \
  -r="designated => identifier \"$helper\" and anchor apple generic" "$app/Helpers/weak"
codesign --force --timestamp --sign "$identity" RequirementFixture.app
for name in resigned renamed adhoc; do cp "$work/fixture" "replacements/$name"; done
codesign --force --timestamp --options runtime --sign "$identity" -i "$helper" replacements/resigned
codesign --force --timestamp --sign "$identity" \
  -i au.edu.vic.woodleigh.stemma.requirement.other replacements/renamed
codesign --force --sign - --timestamp=none -i "$helper" replacements/adhoc
rm -rf "$work"
```
