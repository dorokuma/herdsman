# Slack Access Scope Plan

Date: 2026-06-24

Parent: [Herdsman Herdr Orchestration Plan](../2026-06-24-herdsman-herdr-orchestration.md)

## Status

Archived. Slack access scope MVP is implemented and tested.

## Progress

- **Done** — Team/channel/user allowlists and denial logging were implemented.
- **Done** — Slack config validation requires explicit allowed users.

## Next steps

- Keep this access policy as the baseline for Pi runtime Slack gateway work.

## Goal

用团队、频道、用户 ID 明确控制可以从 Slack 操作 Herdsman 的范围。

Herdsman 是驱动 Herdr 上 agent 和 terminal 的 control-plane，所以不能把它当作对整个 Slack workspace 开放的 bot。MVP 只以 Slack 为对象，Discord / Telegram 等留待以后按 adapter 逐个扩展同一套思路。

## Implementation status

Status as of 2026-06-24 latest `main`: MVP Slack access scope is implemented and covered by tests.

Implemented:

- Slack inbound access policy tests for team / channel / user AND semantics.
- unset allowlists remain unrestricted for that axis.
- bot, edit, delete, and non-message events are ignored before storage.
- `platforms.slack` without `allowed_users` emits a startup warning.
- denied Slack inbound events emit debug logs with reason and IDs, without message text.
- denied inbound events are not stored in the Herdsman DB.
- README Slack setup example includes `allowed_users` and `allowed_channels`, and uses env var names for tokens.

## 现状

`platforms.slack` 有以下设置。

```yaml
platforms:
  slack:
    app_token_env: SLACK_APP_TOKEN
    bot_token_env: SLACK_BOT_TOKEN
    allowed_teams:
      - T123
    allowed_channels:
      - C123
    allowed_users:
      - U123
```

实现上 Slack inbound 也会用 AND 条件检查 `teamId`、`channelId`、`sourceUserId`。未设置的 allowlist 视为不限制。`allowed_users` 未设置时在启动时告警；拒绝时不包含 message text，只在 debug log 里输出理由和 ID。

## 方针

MVP 只把 Slack 作为实现对象。

但命名和职责分离要倾向 platform-neutral。

- core DB 不增加 Slack 专用列。
- platform adapter 负责归一化外部事件，交给 core 时用 `platform`、`spaceId`、`threadId`、`actor.sourceUserId`。
- Slack 的 `team_id` 作为 binding metadata 或 policy 判定用的 platform metadata 处理。
- `allowed_channels` 用于 channel / thread 的入口限制。
- `allowed_users` 用于 DM / channel / thread 全部场景的 sender 限制。
- `allowed_teams` 用于 Slack workspace 边界限制。

不复制 Hermes Agent 复杂的 admin tier 和 pairing flow。Herdsman MVP 只保留静态 YAML 和 `/reload-config`。

## Access Policy Semantics

Slack inbound message 按以下顺序判定。

1. 无法归一化为 Slack message 的事件、bot 自身的 message、编辑 / 删除等一律忽略。
2. 设置了 `allowed_teams` 时，不含 `teamId` 的 message 拒绝。
3. 设置了 `allowed_channels` 时，不含目标 channel 的 channel/thread message 拒绝。
4. 设置了 `allowed_users` 时，sender user ID 不在其中的 message 拒绝。
5. 只有全部通过的 message 才存入 Herdsman session，并触发 gateway turn。

未设置的 allowlist 意味着「该维度不做限制」。但为了安全运维，启用 Slack platform 的配置示例把 `allowed_users` 当作必填。

将来要加强 fail-closed 时，为了不破坏兼容性，用下面任一种方式分阶段引入。

- 只有显式写了 `allow_all_users: true` 才允许没有 user allowlist。
- daemon 启动时对没有 `allowed_users` 的情况告警，并在下一次破坏性变更时改为必填。

## Config Shape

当前维持既有的 shape。

```yaml
platforms:
  slack:
    app_token_env: SLACK_APP_TOKEN
    bot_token_env: SLACK_BOT_TOKEN
    allow_customize: true
    allowed_teams:
      - T1234567890
    allowed_channels:
      - C1234567890
    allowed_users:
      - U1234567890
```

将来的扩展候选:

```yaml
platforms:
  slack:
    allow_all_users: false
    denied_channels:
      - C9999999999
```

`denied_channels` 在 MVP 里不引入。等到需要 allowlist 与 denylist 的优先级时，再作为相当于 Hermes `allowed_channels` / `ignored_channels` 的东西来讨论。

## Delivery Scope

Outbound delivery 遵循既有的 session binding。只有从 Slack inbound 已许可 thread 建立的 binding 才会成为 delivery target，所以通常的 gateway / TUI message 会回到那个 thread。

额外要确认的点:

- 从 TUI 发往 Slack-bound session 的 user message，只会 delivery 到已许可 binding 的 thread。
- 收窄 `allowed_channels` 之后，是否停止对既有 binding 的 outbound delivery，另行判断。

MVP 把 inbound policy 放在最优先，不做既有 binding 的 outbound 失效处理。如果想在 `/reload-config` 后也停止既有 binding 的 delivery，需要另外加 delivery policy。

## Observability

拒绝 Slack event 时，不向用户回复。避免不必要的信息泄露和频道噪声。

daemon log 以 debug level 留下理由。

- `slack policy denied: team`
- `slack policy denied: channel`
- `slack policy denied: user`

不把拒绝事件存入 event stream。目的是不把未许可用户的 message 内容写进 DB。

## Future Adapter Compatibility

追加 Discord / Telegram 时，不要强行抽象 Slack 的配置名。每个 platform 持有自然的 ID。

设想:

```yaml
platforms:
  discord:
    bot_token_env: DISCORD_BOT_TOKEN
    allowed_guilds:
      - "123"
    allowed_channels:
      - "456"
    allowed_users:
      - "789"
    allowed_roles:
      - "999"

  telegram:
    bot_token_env: TELEGRAM_BOT_TOKEN
    allowed_chats:
      - "-100123"
    allowed_users:
      - "123456"
```

core 侧的共通契约只保留以下内容。

- adapter 负责归一化 platform event。
- adapter 在 inbound 保存前判定 platform-specific policy。
- core 只把已许可 message 当作 session event。
- DB binding 用 `platform`, `spaceId`, `threadId`, `metadata` 以 platform-neutral 方式保存。

Discord 的 role 授权和 Telegram 的 group / forum topic 不引入到 Slack MVP。

## Implementation Steps

1. [x] 用测试固定 Slack access policy 的契约。
   - team / channel / user 一致时会被保存。
   - 只要有一个不在 allowlist 内就不保存。
   - 未设置的 allowlist 不限制该维度。
   - bot message / edit / delete 不保存。

2. [x] 增加启动时 validation / warning。
   - `platforms.slack` 有效且没有 `allowed_users` 时告警。
   - 将来引入 `allow_all_users` 时，在这一步调整 schema 和 warning。

3. [x] 增加拒绝理由的 debug log。
   - 不 log message text。
   - 只输出 team/channel/user ID 和拒绝理由。

4. [x] 更新 docs / example config。
   - Slack setup 示例包含 `allowed_users` 和 `allowed_channels`。
   - 明确写出「YAML 里只写 env var name 而不写 token」的方针。

5. [x] 通过 `pnpm check`。

## Non-goals

- Discord / Telegram adapter 实现。
- Hermes Agent 的 pairing flow。
- role-based authorization。
- Slack workspace 管理 UI。
- 未许可 inbound message 的 DB 保存。
- config reload 后对 outbound delivery 的 retroactive blocking。

## Open Questions

- 将来要把没有 `allowed_users` 的 Slack config 做成 hard error，还是只停留在告警？
- Herdsman 是否也明确写出「`allowed_channels` 不适用于 DM」这一规格？当前基于 Slack channel ID，所以只要把 DM channel 放进 allowlist 就能限制；但 Hermes 把 DM 排除在 channel allowlist 对象之外。
- `/reload-config` 后，是否也应该用新 policy 停止对既有 Slack binding 的 outbound delivery？
