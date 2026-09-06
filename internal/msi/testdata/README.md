# MSI fixture

Rebuild from this directory with [wixl](https://github.com/GNOME/msitools)
(`brew install msitools` or `sudo apt install wixl msitools`):

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
