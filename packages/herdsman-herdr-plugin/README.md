# herdsman-herdr-plugin

Herdr companion plugin for Herdsman agent history. Herdr installs this integration from the Herdsman GitHub repository; it is not published to npm.

Install the plugin from a release tag:

```bash
herdr plugin install ryonakae/herdsman/packages/herdsman-herdr-plugin --ref v0.4.0 --yes
```

The plugin requires the Herdsman CLI and daemon:

```bash
npm install --global @dorokuma/herdsman
herdsman daemon start
```

It shows compact rows from `herdsman agent list` for the current Herdr workspace and uses the daemon RPC method `agent.list` with `HERDR_WORKSPACE_ID`. Each row keeps the optional Herdr live name separate from the runtime agent kind and includes the latest cached user and assistant excerpts.
