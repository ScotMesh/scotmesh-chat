# The chat page's banner

The NomadNet chat page shows a small banner above its title. With nothing
configured, that's the Saltire — built into the binary, so it works with no
setup at all. `[page] banner_file` (or `SCOTMESH_CHAT_PAGE_BANNER_FILE`)
replaces it with a file of your own, or removes it.

```
[page]
banner_file = "banner.mu"    # relative to data_dir, or an absolute path
```

| `banner_file` | Result |
|---|---|
| unset (the default) | the Saltire |
| a file with content | that file's lines, in place of the Saltire |
| an empty file, or `/dev/null` | no banner at all; the page uses a compact one-line title |

A visitor can still hide whichever banner is configured with the page's
"Flag" button (or `/page flag off`), the same as today; that only toggles
visibility; it doesn't change what the banner is. With no banner configured,
that button and its "flag" text disappear from the page, since there's
nothing to show or hide.

## Writing the file

The file is [Micron](https://reticulum.network/manual/nomadnetwork.html)
markup — the NomadNet page language, plain text with backtick-escaped
formatting codes. Colours and backgrounds are the two most useful:

- `` `B05b `` sets the background colour (a 3-digit hex triplet).
- `` `F fff `` sets the text (foreground) colour.
- `` `b `` and `` `f `` reset the background and foreground back to default.

Each line of the file is rendered as its own line on the page, exactly as
written, so it carries its own formatting. Nothing in the file is escaped:
it's operator content, on the same footing as the RRC greeting or a room's
topic in the config file, and it can use anything Micron supports.

The built-in Saltire (`internal/nomadpage/banners/saltire.mu`) is a working
example, five rows of block characters on a blue background:

```
`B05b`Ffff████            ████`f`b
`B05b`Ffff    ████    ████    `f`b
`B05b`Ffff        ████        `f`b
`B05b`Ffff    ████    ████    `f`b
`B05b`Ffff████            ████`f`b
```

## Limits

The banner is sent with every page load, including over LoRa, so it's kept
small: at most 24 lines and 4 KB. A line may not start with `#!` (that's a
NomadNet page directive, not banner content). `scotmesh-chat --check-config`
validates the file — including its size and line count — before the hub
starts, so a mistake is caught immediately rather than showing up as a
malformed page.

A reasonable width is 20–40 columns: NomadNet's window is often no wider
than that on a phone, and Reticulum links are frequently slow.
