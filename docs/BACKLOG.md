# Backlog

## Dynamic upstream updates

- [x] Принимать legacy `notifications/tools/list_changed`, `notifications/prompts/list_changed` и `notifications/resources/list_changed` от upstream.
- [ ] После уведомления повторно запрашивать соответствующий list upstream и применять allowlist/metadata patches.
- [ ] Атомарно добавлять, заменять и удалять локальные tool/prompt/resource handlers после refresh.
- [ ] Отправлять downstream-клиенту соответствующее `*_list_changed` только после успешного refresh.
- [ ] Поддержать `notifications/resources/updated` и relay resource subscriptions.
- [ ] После обновления Go и MCP SDK перейти на MCP 2026-07-28 `subscriptions/listen`; сохранить legacy notifications для старых upstream.
- [ ] Добавить file watcher для `.mcp-filter.json` и `.mcp-filter.local.json`, с debounce и безопасным rollback при невалидной конфигурации.

## Metadata and protocol transparency

- [ ] Расширить `metadata.patches` на `initialize`, `prompts/get`, `resources/read` и безопасные поля прочих ответов.
- [ ] Добавить строгую проверку method/select/patch в JSON Schema и runtime validation.
- [ ] Пересылать разрешённые upstream notifications, не относящиеся к list changes, когда их semantics поддерживается proxy.

## Reliability and security

- [ ] Добавить timeout подключения, retry/backoff для Streamable HTTP и SSE, без ограничения долгоживущего SSE потока общим HTTP timeout.
- [ ] Добавить тесты redaction для URL credentials, headers и stderr upstream.
- [ ] Добавить ограничение параллелизма и диагностические JSON-RPC errors для недоступного upstream.

## Delivery

- [ ] Обновить Go и Go MCP SDK до актуальных совместимых версий; выполнить compatibility suite всех transport-ов.
- [ ] Добавить version injection, changelog, release workflow, архивы и checksums для macOS/Linux/Windows.
