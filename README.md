# mcp-filter

`mcp-filter` — локальный stdio MCP-прокси на Go. Он оборачивает отдельный «настоящий» MCP-сервер, показывает клиенту только разрешённые инструменты и перенаправляет вызовы без изменения их аргументов и результата.

Prompts, resources и resource templates передаются без фильтрации; их list metadata можно изменить через `metadata.patches`.

Проект предназначен для команд, которые используют большие MCP-серверы в Codex и Claude, но не хотят оплачивать контекст десятками ненужных JSON Schema. Правила доступа и metadata-overlays задаются один раз в проектном конфиге, а личные дополнения — отдельно и не попадают в VCS. Команды, URL, аргументы и другие параметры подключения остаются в нативных MCP-конфигах клиентов.

Подробная продуктовая спецификация, предполагаемый формат конфигурации и план работ: [docs/PRODUCT.md](docs/PRODUCT.md).

Оставшиеся задачи и прогресс по динамическим обновлениям: [docs/BACKLOG.md](docs/BACKLOG.md).

Стартовый конфиг: [examples/.mcp-filter.json](examples/.mcp-filter.json). Для запуска вне корня проекта передайте явный путь: `mcp-filter tracker --config /path/.mcp-filter.json -- …`.

`tracker` в командах ниже — имя MCP-сервера, то есть ключ в `mcpServers`. Проверка без запуска upstream: `mcp-filter validate tracker`. Для сверки allowlist с фактическим сервером: `mcp-filter validate --check-upstream tracker -- npx -y @acme/tracker-mcp`. `inspect` подключается к upstream и выводит tools со статусом `published` или `hidden`; `version` печатает версию бинарника.

По умолчанию вызов upstream-инструмента ограничен 120 секундами. Измените предел флагом `--timeout 30s`; `--timeout 0` отключает ограничение.

## Логи

Логи настраиваются один раз для всех proxy в `.mcp-filter.json`; в `.mcp-filter.local.json` можно переопределить отдельные поля. У каждого MCP entry — свой append-only файл в указанной папке. Ротация пока намеренно не выполняется.

```json
{
  "logging": {
    "directory": ".mcp-filter/logs",
    "level": "info",
    "format": "json"
  },
  "mcpServers": {}
}
```

`directory` разрешается относительно каталога rules-файла. Поддерживаются уровни `error`, `warn`, `info`, `debug` и форматы `text`, `json`; по умолчанию — `warn` и `text`. Если раздел `logging` отсутствует, предупреждения и ошибки остаются в `stderr`, а файлов не создаётся. В логи не попадают аргументы и результаты вызовов, HTTP-заголовки и значения окружения.

Прокси всегда объявляет capability `listChanged` для tools, prompts и resources. Изменения `.mcp-filter.json` или `.mcp-filter.local.json` отслеживаются во время работы: allowlist и metadata tools перезагружаются с debounce, а подключённым клиентам отправляется `notifications/tools/list_changed`. Некорректная редакция не заменяет уже работающие правила. Если entry отсутствует в обоих rules-файлах, сервер работает прозрачным pass-through; добавление entry начинает фильтрацию без перезапуска, удаление возвращает pass-through.

Готовые обёртки: [Claude `.mcp.json`](examples/claude.mcp.json) и [Codex `config.toml`](examples/codex.config.toml). Имя entry обычно совпадает с ключом `mcpServers` в rules-конфиге; отсутствующий ключ намеренно означает pass-through.

Для HTTP URL transport по умолчанию — `auto`: сначала используется Streamable HTTP, а legacy SSE выбирается только после подтверждения legacy SSE handshake. Укажите `--transport streamable-http` или `--transport sse`, чтобы жёстко выбрать протокол. Для команды после `--` по-прежнему используется stdio.

## Целевой сценарий

1. Нативные `.mcp.json` и `config.toml` по-прежнему регистрируют реальные MCP-серверы и содержат их команды, URL, аргументы и параметры подключения.
2. В каждом таком entry исходный launcher заменяется на `mcp-filter`, а исходная команда или URL передаётся ему аргументами.
3. Репозиторий содержит коммитящийся `.mcp-filter.json` с правилами по именам серверов: allowlist и metadata-overlays.
4. Разработчик создаёт `.mcp-filter.local.json` для личных правил; файл добавлен в `.gitignore`.

Конфигурация подключения не переносится в `mcp-filter`; общий источник правды — только правила фильтрации и метаданные. Это сохраняет привычное место регистрации каждого MCP-сервера и не вводит вторую модель transport-конфигурации.

## Статус

На данный момент в репозитории находится продуктовая спецификация и план; реализация ещё не начата.
