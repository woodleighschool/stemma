# Expressions

Use `{{ expression }}` in YAML values to select data, supply environment values or
compose text. Expressions use CEL, with no filesystem, network or process access.
Quote expressions in YAML:

```yaml
client_secret: "{{ env.CLIENT_SECRET }}"
version: "{{ facts.app.app.version }}-1"
description: "Vendor release {{ evidence['vendor.release'].version }}"
```

## Values and text

An expression occupying the whole YAML value retains its result type: string,
boolean, number, null, list or object. Surrounding text produces a string; embedded
expressions must return strings, booleans or numbers. An embedded null, list or
object is an error.

```yaml
enabled: "{{ env.CHANNEL == 'production' }}"
keep: "{{ int(env.RETAIN_VERSIONS) }}"
description: "Release {{ evidence['vendor.release'].version }}"
```

Environment values are always strings. Convert them explicitly when the target
field needs another type. Expressions do not run again on their results.

Escape a literal opener with a backslash. YAML single quotes or block text retain
that backslash:

```yaml
description: '\{{ not_an_expression }}'
```

This publishes the literal text `{{ not_an_expression }}`. Ordinary shell
substitutions inside script text remain shell syntax.

## Available data

| Context    | Contents                                                                                  | Available when                                |
| ---------- | ----------------------------------------------------------------------------------------- | --------------------------------------------- |
| `env`      | Referenced process environment values                                                     | Loading, preparation and destination metadata |
| `facts`    | Selected inspection subjects, with their native `app`, `package` or other evidence fields | After software preparation                    |
| `evidence` | Namespaced metadata supplied with the prepared artifact                                   | Destination metadata                          |
| `inputs`   | Named builder inputs: `version`, `filename`, `sha256`, `size`, `format` and `evidence`    | Builder preparation                           |

Names containing punctuation use brackets, such as
`evidence['vendor.release'].version`. Builder inputs expose portable metadata,
not runner paths. A downloaded input may have no declared version.

Software kinds expose inspected subjects and can name selections for destination
metadata. Their fields retain the
inspection model's meanings: for example, `app.version` is the application's
short version and `app.build` is its separate build version.

## Missing and null values

A required lookup fails if its key is absent. Use CEL optional selection when an
absent value has a deliberate fallback:

```yaml
developer: "{{ evidence.?vendor.name.orValue('Unknown') }}"
```

An explicit null is present and remains null; `orValue` does not replace it. Where
null also needs a fallback, test it explicitly:

```yaml
developer: "{{ evidence.vendor.name == null ? 'Unknown' : evidence.vendor.name }}"
```

## Evaluation boundaries

Project settings and source declarations resolve their environment expressions
while loading. `BuildMacPkg` resolves package fields,
payload and script entries after acquiring its inputs. Destination metadata
resolves after software preparation. Facts cannot select what must be
acquired before those facts exist.

Document identities, object keys, `$input` names, resolver and operation names,
component references and resource references remain literal. The authored schema
checks declaration structure and expression syntax. Once data is available, the
resolved values must satisfy the field's native schema and semantic validation.
Destination reports identify expression-supplied fields with origin `expression`.

Expressions apply to authored YAML values, including multiline `content` and
destination scripts. Files selected with `$input`, such as a repository
`Scripts/postinstall`, are copied unchanged. Stemma never executes installer or
policy scripts while evaluating expressions or building packages.

CEL's string functions can encode text for its destination format. For a shell
argument, surround the result with single quotes and escape embedded single
quotes:

```yaml
postinstall_script: |-
  #!/bin/sh
  /usr/local/bin/example --token '{{ env.EXAMPLE_TOKEN.replace("'", "'\\''") }}'
```

Values containing dollar signs, command substitutions or newlines remain one
literal shell argument. Stemma's interpolation alone does not provide shell,
XML or other destination-specific escaping.
