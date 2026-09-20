# ccx

Switch which Claude subscription account Claude Code uses.

Claude Code reads one login off disk and holds it. `ccx` saves each account you have logged into and swaps between them: `ccx use <name>` rewrites the on-disk login, so your next Claude Code session runs as that account. That much needs no proxy. Run the bundled proxy and point Claude Code at it and the switch goes live instead: the proxy carries every saved account's token and swaps which one it forwards per request, so you change accounts mid-session with no restart, and when one hits its rate limit it rolls to the next and hides the 429.

**Windows only for the autostart bits.** Everything else is plain Go and runs anywhere; `install` / `restart` / `uninstall` drive a Windows Scheduled Task.

## Install

Needs Go 1.24+.

```bash
go install github.com/Minoxs/claude-code-switcher/cmd/ccx@latest
```

That drops a `ccx` binary in your `GOBIN`.

## Quick start

Accounts come from Claude Code itself. Log into one, save it, log into the next, save that one. `ccx add` snapshots whatever is logged in on disk right now.

```powershell
# log into an account in Claude Code, then:
ccx add work
# log into another account, then:
ccx add personal

ccx use personal          # rewrite the on-disk login; the next session is personal
ccx list                  # saved accounts, * marks the chosen one
```

Without the proxy that is the whole story: `ccx use` swaps the login and a fresh Claude Code picks it up. For live switching inside a running session, start the proxy and point Claude Code at it:

```powershell
ccx serve                 # start the proxy (leave it running)
ccx env | iex             # point THIS shell's Claude Code at the proxy
                          # (ccx env bash  for a POSIX shell)

ccx use personal          # now flips the live session mid-conversation
```

`ccx env` only sets `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` for the shell you run it in. The auth token is a dummy the proxy ignores; the real tokens never leave the proxy.

## How it works

With the proxy in front it is the only thing that talks to Anthropic. Claude Code sends it a request with a throwaway bearer, the proxy rewrites that to the chosen account's real OAuth token, re-adds the subscription OAuth capability Claude Code's gateway mode strips, and forwards it. It is also the sole refresher of every account's token, so refresh-token rotation never races the Claude Code you have open.

`ccx use <name>` picks your preferred account and that choice sticks. Rotation on a rate limit is a per-request fallback: if the chosen account is cooling down the proxy serves from another one, but it does not repoint your selection, so the moment your account's window reopens it goes back to using it. When every account is limited it parks on whichever resets soonest and hands the 429 back rather than looping.

Errors the proxy would otherwise swallow (a token refresh that fails, an upstream that rejects a prime) land in `~/.claude/switcher/proxy.log`. The autostart task runs with no console, so that file is where you look when an account stops serving.

## Commands

```
ccx add [name]           save the currently logged-in account as a profile
ccx list                 saved accounts, * on the chosen one
ccx current              the account logged in on disk right now
ccx use <name>           choose <name>; a running proxy flips to it live
ccx rm <name>            delete a saved profile
ccx serve [--port N]     run the proxy; --autostart to stagger account windows
ccx usage [--refresh]    per-account rate-limit usage; --refresh re-pings open windows
ccx env [bash]           print the env vars pointing Claude Code at the proxy
ccx ping [-v]            exit 0 if the proxy is up
ccx install              start the proxy at logon (Windows Scheduled Task)
ccx restart              restart the installed proxy on the current binary
ccx uninstall            remove the logon task
```

## Autostart

Anthropic resets a subscription's usage window five hours after its first request, so an idle account's budget is only budget once you have spent a request to open the window. `--autostart` sends each saved account a minimal priming request and staggers them by `5h / N`, so their windows reset at different times instead of all at once and you always have an account with headroom.

`ccx install` registers a logon task that runs `ccx serve --hidden --autostart`, so the proxy is up before you open Claude Code and no console window shows. **Windows only.** After rebuilding the binary, `ccx restart` repoints the task at it and cycles the proxy without a logout. `ccx uninstall` drops the task.

## Configuration

Everything has a working default. Set these only to change behaviour.

| variable | default | meaning |
|---|---|---|
| `CCX_PORT` | `8787` | proxy listen port, and where `ccx use` / `usage` / `ping` look for it |
| `CCX_UPSTREAM` | `https://api.anthropic.com` | backend the proxy forwards to |
| `CCX_AUTOSTART` | | `1` or `true` makes a bare `ccx serve` behave like `--autostart` |
| `CCX_OAUTH_BETA` | `oauth-2025-04-20` | the OAuth capability re-added to each forwarded request |
| `CCX_OAUTH_CLIENT_ID` | built-in | OAuth client id used to refresh account tokens |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | where profiles, the chosen account, usage snapshots, and the log live |

## AI disclosure

This is entirely AI-written. It is a personal tool I built for myself and cleaned up enough to share, not production-ready software. I have read through it, but it holds real subscription tokens, so use it at your own risk.
