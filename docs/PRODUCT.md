# Продуктовая спецификация: mcp-filter

## Проблема и ценность

MCP-клиенты получают полный `tools/list`: имена, описания и JSON Schema. Большой upstream часто предоставляет десятки инструментов, хотя команде нужны два-три. Это расходует контекст до первой задачи и ухудшает выбор инструмента моделью.

`mcp-filter` — Go-реализация идеи [pro-vi/mcp-filter](https://github.com/pro-vi/mcp-filter), но с конфигурацией правил, независимой от транспорта. Реальные серверы продолжают регистрироваться там, где ожидает клиент: Claude — в `.mcp.json`, Codex — в `config.toml`. Эти файлы содержат команды, URL, аргументы, заголовки и окружение. `.mcp-filter.json` содержит только policies и metadata-overlays.

| Кто | Результат |
| --- | --- |
| Разработчик | Меньше контекста и только полезные инструменты. |
| Команда | Версионируемые allowlist и единые улучшенные метаданные. |
| Интегратор | Нет второго хранилища transport-параметров и секретов. |

## Архитектура

Каждый зарегистрированный MCP-сервер оборачивается независимо. `mcp-filter proxy` — stdio MCP-server для хоста и MCP-client ровно для одного upstream.

```text
Codex / Claude config
  entry "tracker": mcp-filter proxy --entry tracker -- stdio-command …
                                      │
                                      ├─ .mcp-filter.json: tracker rules
                                      └─ tracker MCP upstream
```

Прокси при старте подключается к upstream, получает его `tools/list`, накладывает rules для `entry` и публикует только разрешённые записи. Исходные имена инструментов сохраняются. `tools/call` с разрешённым именем передаётся upstream с исходными аргументами; результат, ошибка инструмента и все значения вызова передаются без изменения.

В MVP поддерживаются upstream-транспорты stdio, Streamable HTTP и legacy SSE. Внешний транспорт всегда stdio, поэтому remote entry клиента меняется с `url` на `command = "mcp-filter"`: исходный URL и его transport-опции становятся аргументами этого process в том же клиентском конфиге. Это технически необходимо, чтобы локальный процесс мог быть посредником; URL не перемещается в filter-config.

## Границы MVP

Входит:

- stdio-прокси для MCP-клиентов;
- stdio, Streamable HTTP и legacy SSE upstream;
- точный allowlist по имени инструмента; отсутствие `allow` означает публикацию нуля инструментов;
- `.mcp-filter.json` + `.mcp-filter.local.json`, где local игнорируется Git;
- независимые overlays метаданных сервера и инструментов;
- `validate`, `inspect`, stderr-логи и безопасные ошибки;
- тесты конфигурации, transport-adapter-ов, фильтрации и прозрачной передачи вызовов;
- релизы для macOS, Linux и Windows под лицензией MIT.

Не входит: изменение values аргументов вызова или результата инструмента; policies по значениям аргументов; фильтрация prompts/resources, schema pruning, OAuth-терминация, UI и метрики.

## Конфигурация правил

Процесс ищет вверх от working directory `.mcp-filter.json` и соседний `.mcp-filter.local.json`.

Приоритет: `CLI flags > local > base > secure defaults`. Слияние идёт по имени entry и имени инструмента. `allow` заменяется целиком (не объединяется), чтобы local-файл не расширял права случайно. Любой metadata object накладывается как JSON Merge Patch: local может дополнить или заменить поля base. Конфиг не содержит `command`, `args`, `url`, `headers`, `env`, `cwd`, OAuth или transport.

```json
{
  "$schema": "https://mcp-filter.dev/schema/v1.json",
  "entries": {
    "tracker": {
      "allow": ["get_issue", "search_issues"],
      "metadata": {
        "server": {
          "instructions": "Инструменты трекера для текущего проекта."
        },
        "tools": {
          "get_issue": {
            "description": "Получить задачу по ключу; используй перед изменением кода.",
            "annotations": { "readOnlyHint": true }
          },
          "search_issues": {
            "description": "Поиск задач в проекте."
          }
        }
      }
    }
  }
}
```

`allow` и `metadata.tools` независимы: можно временно убрать `search_issues` из allowlist, не удаляя его описание. Если upstream позже вновь публикует этот инструмент, overlay автоматически снова применяется.

### Metadata overlays

`metadata.server` и `metadata.tools.<name>` — краткая форма для наиболее частых случаев. Они позволяют подменять или дополнять `description`, `title`, `annotations`, `icons`, `_meta`, `inputSchema`, `outputSchema`, `instructions` и будущие поля протокола.

Для любой другой metadata вводится универсальный `metadata.patches`: массив `{ "method", "select", "patch" }`. `method` выбирает upstream-ответ (сейчас поддержаны `tools/list`, `prompts/list`, `resources/list`, `resources/templates/list`), `select` выбирает элемент по устойчивому идентификатору, а `patch` накладывается как JSON Merge Patch. Это позволяет менять metadata prompts и resources без добавления новой схемы на каждое поле. Такой overlay не означает фильтрацию этих primitives: prompts/resources в MVP передаются как есть.

Патч применяется только к неисполняемой части структур ответа upstream, не к значениям tool call/results.

Идентификаторы и маршрутизация не являются metadata: `tool.name`, JSON-RPC `id`, `method`, `params.name` при `tools/call` и значения результатов сохраняются. Смена `tool.name` намеренно остаётся вне MVP, чтобы не разрывать прозрачность вызова и не создавать коллизии.

## Обёртки в конфигурации клиентов

Имя `entry` должно совпадать с именем сервера в config клиента.

### stdio upstream

Было:

```json
"tracker": { "command": "npx", "args": ["-y", "@acme/tracker-mcp"] }
```

Станет:

```json
"tracker": {
  "command": "mcp-filter",
  "args": ["proxy", "--entry", "tracker", "--transport", "stdio", "--", "npx", "-y", "@acme/tracker-mcp"]
}
```

### Streamable HTTP/SSE upstream

Было: `url = "https://mcp.example.com/mcp"`.

Станет: stdio entry с `command = "mcp-filter"` и аргументами `proxy --entry example --transport streamable-http --url https://mcp.example.com/mcp`; legacy SSE использует `--transport sse`. Заголовки и источники токенов остаются явными параметрами этого entry, например `--header-env AUTHORIZATION`; в filter-config они не попадают. Реальный синтаксис Codex/Claude примеров фиксируется и тестируется на этапе интеграции.

## Требования безопасности и качества

- stdout — только MCP JSON-RPC; логи и upstream stderr — в stderr;
- секреты, URL credentials и значения заголовков не журналируются и не выводятся в `inspect`;
- неизвестный или запрещённый инструмент не вызывается;
- upstream, entry и transport включаются в ошибку, но без секретов;
- порядок инструментов стабилен;
- интеграционные тесты проверяют сохранение структуры/значений вызовов;
- матрица релизов: darwin/linux/windows, amd64/arm64 где применимо; MIT.

## План разработки

### Этап 0 — контракт и compatibility spike

1. Утвердить schema rules-конфига и CLI-форму трёх transport-адаптеров.
2. Выбрать Go MCP SDK, подтвердив stdio server/client, Streamable HTTP client и SSE client.
3. Зафиксировать golden MCP сообщения для metadata merge и errors.

### Этап 1 — конфигурация и CLI

1. Go module и команды `proxy`, `validate`, `inspect`, `version`.
2. Поиск base/local, слияние policies, JSON Schema, JSON Merge Patch и redaction.
3. Unit-тесты: allowlist независимо от overlays, local priority, пустой allowlist.

### Этап 2 — прозрачный stdio wrapper

1. Запуск upstream после `--`, lifecycle и cancellation.
2. Фильтрация `tools/list`, metadata merge и прозрачный `tools/call`.
3. Integration-test с fake MCP server.

### Этап 3 — remote adapters

1. Streamable HTTP upstream, headers/env references и timeouts.
2. Legacy SSE upstream.
3. Contract/integration tests с test servers.

### Этап 4 — поставка

1. Проверка реальных Codex и Claude конфигов.
2. CI: format, lint, unit, integration, race и release matrix.
3. Documentation, migration guide, samples для трёх transport-типов.

### Этап 5 — обновление Go и MCP SDK

1. Обновить Go до актуальной стабильной версии, которую требует поддерживаемый официальный Go MCP SDK.
2. Обновить `github.com/modelcontextprotocol/go-sdk` до актуальной стабильной версии.
3. Прогнать compatibility suite для stdio, Streamable HTTP и legacy SSE upstream, а также smoke tests с Codex и Claude.
4. Зафиксировать обновлённые минимальные версии в `go.mod`, CI и документации; описать breaking changes в migration guide.

## Критерии готовности MVP

- один существующий stdio entry и один HTTP/SSE entry успешно оборачиваются без записи transport-настроек в `.mcp-filter*.json`;
- 100% опубликованных инструментов входят в явный `allow`;
- metadata инструмента сохраняется при его временном удалении из allowlist;
- вызов разрешённого инструмента сохраняет аргументы и результат upstream;
- macOS, Linux и Windows release binaries проходят smoke test.
