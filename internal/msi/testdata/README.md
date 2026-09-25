# MSI fixtures

`test.msi` carries product properties only; `icon.msi` also registers `icon.ico`,
a 128-pixel PNG-framed ICO, as its `ARPPRODUCTICON`. Rebuild from this directory
with [wixl](https://github.com/GNOME/msitools) (`brew install msitools` or
`sudo apt install wixl msitools`):

```sh
work=$(mktemp -d)
printf 'Stemma fixture\n' > "$work/fixture.txt"
cat > "$work/test.wxs" <<'XML'
<Wix xmlns="http://schemas.microsoft.com/wix/2006/wi">
  <Product Id="8B2D32B7-0BE9-4CF9-B1E7-42C27753A6B8" Name="Stemma MSI Fixture"
    Version="1.2.3" Manufacturer="Woodleigh School" Language="1033"
    UpgradeCode="A3C857D8-3CF2-4FDB-A751-1D74152E7B7D">
    <Package InstallerVersion="500" Compressed="yes" InstallScope="perMachine"/>
    <Media Id="1" Cabinet="fixture.cab" EmbedCab="yes"/>
    <Directory Id="TARGETDIR" Name="SourceDir">
      <Directory Id="ProgramFiles64Folder">
        <Directory Id="INSTALLDIR" Name="Stemma MSI Fixture">
          <Component Id="Payload" Guid="AB6C0714-59CF-4A29-9F38-4C0270F17AF3" Win64="yes">
            <File Id="FixtureFile" Source="fixture.txt" KeyPath="yes"/>
          </Component>
        </Directory>
      </Directory>
    </Directory>
    <Feature Id="Main" Level="1"><ComponentRef Id="Payload"/></Feature>
  </Product>
</Wix>
XML
(cd "$work" && wixl -a x64 -o test.msi test.wxs)
msibuild "$work/test.msi" -s 'Stemma MSI Fixture' 'Woodleigh School' 'x64;1033' \
  '{71C6B8B7-EF12-4C0B-A390-AD3899831AFA}'
cp "$work/test.msi" test.msi
rm -rf "$work"
```

For `icon.msi`, change the product to `Id="3F0B1C7E-5D2A-4B8E-9C6F-1A2B3C4D5E6F"`,
`Name="Stemma Icon Fixture"`, `UpgradeCode="B7E6D5C4-3B2A-4F1E-8D9C-0A1B2C3D4E5F"`
and the component to `Guid="C1D2E3F4-A5B6-4C7D-8E9F-0A1B2C3D4E5F"`, add

```xml
<Icon Id="ProductIcon.ico" SourceFile="icon.ico"/>
<Property Id="ARPPRODUCTICON" Value="ProductIcon.ico"/>
```

after the `Media` element with `icon.ico` copied beside the source, and stamp the
summary with `'Stemma Icon Fixture'` and `'{9E8D7C6B-5A4F-4E3D-2C1B-0A9F8E7D6C5B}'`.

`files.msi` has a File table with a 2-byte `Sequence` column and versioned,
unversioned, companion, duplicate and malformed rows. Build it like `test.msi`
with the product `Id="5E0C7A1B-2D3F-4A6B-9C8D-7E6F5A4B3C2D"`,
`Name="Stemma File Fixture"`, `UpgradeCode="D4C3B2A1-9F8E-4D7C-8B6A-5F4E3D2C1B0A"`
and the component `Guid="E5F6A7B8-C9D0-4E1F-A2B3-C4D5E6F7A8B9"`, then replace its
File table before stamping the summary with `'Stemma File Fixture'` and
`'{0B1C2D3E-4F5A-4B6C-8D7E-9F0A1B2C3D4E}'`:

```sh
{
  printf 'File\tComponent_\tFileName\tFileSize\tVersion\tLanguage\tAttributes\tSequence\n'
  printf 's72\ts72\tl255\ti4\tS72\tS20\tI2\ti2\nFile\tFile\n'
  printf 'FixtureFile\tPayload\tfixture.txt\t15\t\t\t512\t1\n'
  printf 'VendorExe\tPayload\tVENDOR~1.EXE|Vendor.exe\t1024\t7.2.1.48556\t1033\t512\t2\n'
  printf 'HelperDll\tPayload\thelper.dll\t1024\tVendorExe\t\t512\t3\n'
  printf 'SharedX64\tPayload\tshared.dll\t1024\t1.0.0.0\t1033\t512\t4\n'
  printf 'SharedArm64\tPayload\tSHARED.DLL\t1024\t1.0.0.0\t1033\t512\t5\n'
  printf 'ToolExe\tPayload\ttool.exe\t1024\t6.2.8-948\t1033\t512\t6\n'
} > "$work/File.idt"
msibuild "$work/files.msi" -q 'DROP TABLE `File`'
msibuild "$work/files.msi" -i "$work/File.idt"
```
