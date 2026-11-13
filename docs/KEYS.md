# Key bindings

_Generated from the built-in defaults by `go test ./internal/tui/keymap/ -update`.
Do not edit by hand: change the bindings, then regenerate._

Every binding below can be overridden in `keymap.yaml` in the configuration
directory (`~/.config/jumper/keymap.yaml`) by its name, without rebuilding
jumper. The prefix key itself is `mux.prefix` in `config.yaml` (default `C-a`).

A key list replaces the default list completely, so name every key you want
to keep. An unknown name or a key bound to two actions is a configuration
error naming the line to fix; `:keys` shows the map jumper is running with.

## View and global bindings

These work inside a view pane without the prefix.

| Name | Keys | Action |
|---|---|---|
| `up` | k, up | up |
| `down` | j, down | down |
| `page_up` | pgup, C-u | page up |
| `page_down` | pgdown, C-d | page down |
| `top` | g, home | top |
| `bottom` | G, end | bottom |
| `open` | enter | open |
| `back` | esc | back |
| `command` | : | command |
| `filter` | / | filter |
| `fuzzy_find` | C-f | fuzzy find |
| `help` | ? | help |
| `refresh` | r | refresh |
| `quit` | q | quit |
| `actions` | x | actions |
| `note` | n | note |
| `finding` | F | finding |
| `knowledge` | K | knowledge |
| `timeline` | T | timeline |
| `yank` | y | yank |
| `follow` | w | follow |
| `time_window` | t | time window |
| `diff` | d | diff |
| `errors` | e | errors |

## Prefix bindings

These follow the prefix key `C-a` in the format `C-a <key>`,
and are listed under `prefix:` in `keymap.yaml`.

| Name | Keys |
|---|---|
| `command` | : |
| `copy-mode` | [ |
| `detach` | d |
| `focus-down` | j, down |
| `focus-last` | ; |
| `focus-left` | h, left |
| `focus-next` | o |
| `focus-right` | l, right |
| `focus-up` | k, up |
| `help` | ? |
| `kill-pane` | x |
| `kill-window` | & |
| `layout-next` | space |
| `new-window` | c |
| `next-window` | n |
| `prev-window` | p |
| `rename-session` | $ |
| `rename-window` | , |
| `resize-down` | J |
| `resize-left` | H |
| `resize-right` | L |
| `resize-up` | K |
| `select-window-0` | 0 |
| `select-window-1` | 1 |
| `select-window-2` | 2 |
| `select-window-3` | 3 |
| `select-window-4` | 4 |
| `select-window-5` | 5 |
| `select-window-6` | 6 |
| `select-window-7` | 7 |
| `select-window-8` | 8 |
| `select-window-9` | 9 |
| `session-list` | s |
| `split-h` | %, \| |
| `split-v` | ", - |
| `ssh` | S |
| `swap-next` | } |
| `swap-prev` | { |
| `sync` | B |
| `window-list` | w |
| `zoom` | z |
