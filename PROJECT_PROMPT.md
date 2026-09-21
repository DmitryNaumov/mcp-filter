# Стартовый промпт проекта

Реализуй `mcp-filter` — локальный stdio MCP-прокси на Go.

Цель: оборачивать уже зарегистрированные MCP upstream-серверы и публиковать клиентам (Codex, Claude и другим MCP-клиентам) только явно разрешённые инструменты. Команды, URL, аргументы, заголовки и прочие transport-параметры остаются в нативных `.mcp.json`/`config.toml`; `.mcp-filter.json` содержит только allowlist и metadata-overlays. Личные изменения хранятся в `.mcp-filter.local.json`, который не попадает в VCS.

Начни строго с MVP из `docs/PRODUCT.md`:

1. Создай CLI `mcp-filter` с командами `proxy`, `validate`, `inspect`, `version`.
2. `proxy --entry NAME` принимает stdio от хоста и подключается ровно к одному upstream через stdio, Streamable HTTP или legacy SSE. Transport-параметры передаются только CLI-аргументами из client config.
3. `proxy` читает base и local rules, получает upstream `tools/list`, публикует только `allow` и сохраняет исходные имена инструментов.
4. Реализуй независимые `metadata.server` и `metadata.tools.<tool>` как JSON Merge Patch для любой неисполняемой metadata upstream. Не меняй `tool.name`, JSON-RPC id/method, аргументы вызова или значения результатов.
5. На `tools/call` проверяй исходное имя, перенаправляй его и аргументы upstream, не меняя результат.
6. Не пиши ничего, кроме MCP JSON-RPC, в stdout. Логи направляй в stderr. Никогда не логируй значения переменных окружения и секреты.
7. Добавь JSON Schema rules-конфига, sample-конфиг, unit-тесты слияния/валидации и integration-тесты с fake stdio/HTTP/SSE upstream.

Контракт конфигурации, правила слияния, границы и критерии готовности находятся в `docs/PRODUCT.md`; не добавляй regex-фильтры, schema pruning, OAuth-терминацию или resources/prompts filtering до завершения MVP.

Перед кодированием проведи короткий compatibility spike для выбора Go MCP SDK. Зафиксируй версию зависимости и проверь stdio client+server, Streamable HTTP client и legacy SSE client. Если SDK не позволяет прозрачно передавать требуемые MCP-сообщения, объясни блокер и предложи минимальный адаптер, а не меняй протокол молча.
