# Third-party components

clashcli source is distributed under the repository's MIT license. It controls the following independent upstream programs/data. Their licenses are not replaced by clashcli's license.

| Component | Source | License |
|---|---|---|
| mihomo | https://github.com/MetaCubeX/mihomo | GPL-3.0 |
| MetaCubeXD | https://github.com/MetaCubeX/metacubexd | MIT |
| MetaCubeX rule data | https://github.com/MetaCubeX/meta-rules-dat | See upstream and its individual data sources |
| Cobra | https://github.com/spf13/cobra | Apache-2.0 |
| Go YAML | https://github.com/yaml/go-yaml | MIT / Apache-2.0 |
| Go supplementary libraries | https://go.googlesource.com/term and https://go.googlesource.com/sys | BSD-3-Clause |

The published clashcli executable does not embed or link mihomo, MetaCubeXD, or Geo databases. Initialization downloads these components from their upstream releases. See the corresponding source repositories for notices, source code and redistribution requirements.

Copies of the linked Go dependencies' licenses and notices are included in [third_party/licenses](third_party/licenses) and the release's `licenses.tar.gz`. pflag and the Go toolchain use BSD-3-Clause; mousetrap uses Apache-2.0. Keep these notices when redistributing clashcli binaries.
