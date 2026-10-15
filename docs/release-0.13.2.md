# Spynel 0.13.2 release notes

The npm launcher now reloads the installed JavaScript and its dependencies after
successful package replacement, before coordinating native restarts. Startup
offers and explicit updates retain one Node process, the normalized launch
arguments, the startup-check snapshot and one shutdown latch. Already-current
updates still restart without replacing npm files. Replacement, handoff and
coordination failures remain explicit; native protocol and ownership checks stay
in force.

Native npm update handoffs are now published atomically and checked before
returning update exit code 75. An interrupted private request can be replaced
with the complete current request; unsafe files and publication failures return
an explicit retained error. Reading an exit code no longer writes a request.
Supervised and one-shot launches use the same completion boundary.

Process discovery also rechecks unknown instances after a bounded registration
transition window, preventing an instance entering or leaving the registry from
being mistaken for a persistent unregistered legacy server. Live unregistered
applications still fail update preflight. Native exit recorders now register as
diagnostic helpers before reading input, so bookkeeping cannot become an
application restart target.

`/status` now shows the serving process's version directly above the adjacent
instance IDs. Structured status includes the same `version` field. This reports
the running primary's version even when a newer CLI connects to it; an installed
package version alone does not establish what is serving the workspace.

This patch builds on 0.13.1 and retains its native supervision and guarded
direct-provider ownership behavior. It does not include database migrations.

## Existing npm installations: one-time fresh launcher

A launcher already running from 0.12.14 keeps its loaded protocol-1 JavaScript
after npm replaces its files. The 0.13 native runtime requires protocol 2, so an
old live wrapper can refuse coordinated updates even while the installed version
is current. Replacing files cannot retrofit that wrapper: one controlled full
stop and fresh launch is necessary. This procedure interrupts channels and work.

First wait for any npm replacement to finish and let active work finish or arrange
its interruption. Use the existing installation's account, configuration home,
npm executable and prefix. Check `command -v spynel`, `npm prefix --global` and
`npm root --global` to identify them; do not update a different installation.

Stop external respawn before cleanup. Select the existing manager and substitute
its actual saved name:

- Manual: stop the existing session cleanly; shared cleanup below also finds
  caller-controllable verified Spynel processes.
- PM2: `pm2 stop 'EXISTING_APP'` before cleanup, then
  `pm2 restart 'EXISTING_APP'` after installation.
- User systemd: `systemctl --user stop 'EXISTING_UNIT'` before cleanup, then
  `systemctl --user start 'EXISTING_UNIT'` after installation. For an existing
  system unit, use `sudo systemctl stop 'EXISTING_UNIT'` and
  `sudo systemctl start 'EXISTING_UNIT'`, while installing under the account and
  npm prefix that unit actually uses.

From a separate shell, use that installation's command:

```sh
spynel killall
```

This stops **all caller-controllable verified Spynel processes across installations
and workspaces**, plus matching generated startup services, while preserving
workspace data and saved registrations. It interrupts active work, does not prove
that escaped providers or external effects ended, and may report stop failures.
Resolve those failures before proceeding. Do not override the protocol or clear
ownership markers/locks.

For a verified ordinary **global npm installation**, using its original account
and npm executable:

```sh
npm install --global spynel@0.13.2
```

For a custom global prefix, use
`npm install --global --prefix "$spynel_prefix" spynel@0.13.2`, setting
`spynel_prefix` to the verified existing prefix. For a local installation, run
`npm install spynel@0.13.2` from its existing package project instead. Invoke the
launcher from that same prefix/project afterward; do not silently switch to a
different global command or elevate into another account's installation.

Start once through the original launch path. Manual users return to the original
workspace and run `spynel` or their original `spynel serve` arguments. Managed
users start the existing PM2/systemd registration above, retaining the installed
npm launcher, `--automatic-startup`, original workspace/arguments and environment.
Do not add a foreground copy beside a managed instance.

Verify installation with that launcher's `--version`, then verify the serving
primary with `/status`: it must report **0.13.2**, the expected primary and healthy
configured channels. Run `/update check`, then `/update` from a channel or TUI;
the already-current update must complete a ready restart without the legacy
launcher refusal. Recheck `/status` and channel health after reconnecting.
Package metadata or `--version` equality alone does not verify a running launcher.

The separate same-installation plain-CLI `command update` registration limitation
is not fixed here; use the channel/TUI path for restart verification. No VPS is
updated by publication of this release.
