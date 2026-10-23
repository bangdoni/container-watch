# Third-Party Notices

ContainerWatch itself is MIT licensed (see [LICENSE](LICENSE)). This file
records the licenses of the third-party code that ships with it, as required
by those licenses.

## Vendored browser assets

These files are committed under `web/static/` and served by the binary.

| Asset | Version | License | Upstream |
|---|---|---|---|
| `web/static/js/alpine.min.js` | 3.14.8 | MIT | https://github.com/alpinejs/alpine — Copyright (c) 2019-2025 Caleb Porzio and contributors |
| `web/static/js/htmx.min.js` | 2.0.4 | 0BSD | https://github.com/bigskysoftware/htmx |
| `web/static/js/htmx-sse.js` | — | 0BSD (BSD Zero Clause) | https://github.com/bigskysoftware/htmx-extensions — Copyright (c) 2023, Alexander Petros |

The MIT license of Alpine.js requires its copyright notice and permission
notice to be included in copies or substantial portions of the software. It is
reproduced in full below.

The 0BSD ("Zero-Clause BSD") license of htmx and the htmx SSE extension imposes
no obligation to reproduce the notice, but the notices are included here for
completeness.

### Alpine.js — MIT License

```
Copyright (c) 2019-2025 Caleb Porzio and contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

### htmx — Zero-Clause BSD

```
Permission to use, copy, modify, and/or distribute this software for
any purpose with or without fee is hereby granted.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL
WARRANTIES WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES
OF MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE
FOR ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY
DAMAGES WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER
IN AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING
OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
```

### htmx SSE extension — BSD Zero Clause License

```
Copyright (c) 2023, Alexander Petros

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE
FOR ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
```

## Fonts

The web interface ships these font files under `web/static/fonts/`.

| File | Family | License | Upstream |
|---|---|---|---|
| `Inter-Regular.woff2`, `Inter-Medium.woff2`, `Inter-SemiBold.woff2` | Inter | SIL Open Font License 1.1 | https://github.com/rsms/inter — Copyright (c) 2016 The Inter Project Authors |
| `JetBrainsMono-Regular.woff2` | JetBrains Mono | SIL Open Font License 1.1 | https://github.com/JetBrains/JetBrainsMono — Copyright 2020 The JetBrains Mono Project Authors |

The SIL Open Font License 1.1 permits the fonts to be bundled and redistributed
with software provided the font copyright notice and the license accompany the
font files. The full text of the OFL 1.1 is published at
<https://openfontlicense.org> and is shipped alongside the fonts at
`web/static/fonts/OFL.txt`.

The files here are **subsetted** builds: they carry 231 (Inter) and 230
(JetBrains Mono) glyphs covering Latin-1 and common punctuation, rather than
the full upstream character set, and the Inter build is Version 4.001
(git-66647c0bb) rather than the current upstream release. Under the OFL a
subset is a "Modified Version".

This is permitted. The OFL only restricts a Modified Version's use of a
**Reserved Font Name**, and *neither* Inter nor JetBrains Mono declares one —
their copyright statements are simply "Copyright (c) 2016 The Inter Project
Authors" and "Copyright 2020 The JetBrains Mono Project Authors", with no
"with Reserved Font Name" clause. A Modified Version with no Reserved Font
Name may keep the original family name, so the `Inter` and `JetBrains Mono`
family names used in `web/static/css/app.css` are correct as-is.

The subsetted Inter build retains the upstream copyright string in its `name`
table ("Copyright 2016 The Inter Project Authors"), and the font files are
served with the OFL text at `/static/fonts/OFL.txt`.

## Go runtime dependencies

All runtime dependencies are permissively licensed. None is copyleft, so
ContainerWatch can be distributed under MIT without inheriting any additional
obligation. The list is the full `go.mod` runtime graph.

| Module | Version | License |
|---|---|---|
| `golang.org/x/crypto` | v0.58.0 | BSD-3-Clause |
| `golang.org/x/sys` | v0.49.0 | BSD-3-Clause |
| `modernc.org/sqlite` | v1.61.0 | BSD-3-Clause |
| `modernc.org/libc` | v1.79.0 | BSD-3-Clause |
| `modernc.org/mathutil` | v1.7.1 | BSD-3-Clause |
| `modernc.org/memory` | v1.12.1 | BSD-3-Clause |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause |
| `github.com/dustin/go-humanize` | v1.1.0 | MIT |
| `github.com/mattn/go-isatty` | v0.0.24 | MIT |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause |
| `github.com/ncruces/go-strftime` | v1.1.0 | MIT |

`modernc.org/sqlite` is a pure-Go translation of SQLite. It is **not** the
C implementation, so the SQLite public-domain dedication and the C library's
`sqlite3.c` licensing do not apply to the Go translation. The translation
itself is BSD-3-Clause.

The BSD-3-Clause dependencies require that their copyright notice, conditions
and disclaimer be reproduced in redistributions. They are not vendored into
this repository — they are compiled in from the module cache at build time —
and their notices travel with the module sources, as Go's module system
preserves each module's `LICENSE` file.

## Trademarks

"ContainerWatch", "Docker", "Podman", "Alpine.js", "htmx", "Inter" and
"JetBrains Mono" are the property of their respective owners. They are used
here to identify the software and the technologies it works with. No
trademark rights are granted by this license.
