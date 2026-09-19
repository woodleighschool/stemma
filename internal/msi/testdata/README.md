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
