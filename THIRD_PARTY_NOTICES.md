# Third-party notices

Queueframe includes the following third-party software. Each component remains
under its own license; copies of those license texts are stored under
`third_party/licenses/` and are included in the container image.

| Component | Version | License text |
| --- | --- | --- |
| `github.com/dustin/go-humanize` | `v1.0.1` | [`LICENSE`](third_party/licenses/github.com_dustin_go-humanize/LICENSE) |
| `github.com/gocarina/gocsv` | `v0.0.0-20180809181117-b8c38cb1ba36` | [`LICENSE`](third_party/licenses/github.com_gocarina_gocsv/LICENSE) |
| `github.com/google/go-querystring` | `v1.0.0` | [`LICENSE`](third_party/licenses/github.com_google_go-querystring/LICENSE) |
| `github.com/google/uuid` | `v1.6.0` | [`LICENSE`](third_party/licenses/github.com_google_uuid/LICENSE) |
| `github.com/mattn/go-isatty` | `v0.0.24` | [`LICENSE`](third_party/licenses/github.com_mattn_go-isatty/LICENSE) |
| `github.com/ncruces/go-strftime` | `v1.0.0` | [`LICENSE`](third_party/licenses/github.com_ncruces_go-strftime/LICENSE) |
| `github.com/remyoudompheng/bigfft` | `v0.0.0-20230129092748-24d4a6f8daec` | [`LICENSE`](third_party/licenses/github.com_remyoudompheng_bigfft/LICENSE) |
| `github.com/zerodha/gokiteconnect/v4` | `v4.4.2` | [`LICENSE`](third_party/licenses/github.com_zerodha_gokiteconnect_v4/LICENSE) |
| `golang.org/x/crypto` | `v0.54.0` | [`LICENSE`](third_party/licenses/golang.org_x_crypto/LICENSE) |
| `golang.org/x/sys` | `v0.47.0` | [`LICENSE`](third_party/licenses/golang.org_x_sys/LICENSE) |
| `modernc.org/libc` | `v1.74.4` | [`LICENSE`](third_party/licenses/modernc.org_libc/LICENSE), [`third-party notices`](third_party/licenses/modernc.org_libc/LICENSE-3RD-PARTY.md) |
| `modernc.org/mathutil` | `v1.7.1` | [`LICENSE`](third_party/licenses/modernc.org_mathutil/LICENSE), [`mersenne license`](third_party/licenses/modernc.org_mathutil/mersenne-LICENSE) |
| `modernc.org/memory` | `v1.11.0` | [`LICENSE`](third_party/licenses/modernc.org_memory/LICENSE), [`Go`](third_party/licenses/modernc.org_memory/LICENSE-GO), [`mmap-go`](third_party/licenses/modernc.org_memory/LICENSE-MMAP-GO), [`logo`](third_party/licenses/modernc.org_memory/LICENSE-LOGO) |
| `modernc.org/sqlite` | `v1.56.0` | [`LICENSE`](third_party/licenses/modernc.org_sqlite/LICENSE) |

The browser interface also bundles these assets and their original license
texts:

- [Instrument Sans](internal/web/static/fonts/InstrumentSans-LICENSE.txt)
- [IBM Plex Mono](internal/web/static/fonts/IBMPlex-LICENSE.txt)
- [Lucide and Feather icons](internal/web/static/icons-LICENSE.txt)

The dependency versions in this notice match `go.mod`. When dependencies are
updated, update this notice and the corresponding license copies in the same
change.
