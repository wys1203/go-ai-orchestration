# go-ai-orchestration (`gao`)

極致輕量的 headless AI agent：無人值守監看 GitHub issue，套用 Agent Skills，透過 GitHub MCP 工具自主處理，多 issue 併發執行。

- **單一靜態 binary**，只有兩個直接相依：官方 MCP Go SDK、yaml.v3；LLM 端以純 `net/http` 呼叫任何 **OpenAI-compatible** Chat Completions API（OpenAI、OpenRouter、vLLM、Ollama、LM Studio…），不綁定供應商
- **多併發**：每個 repo 一個輪詢 goroutine，全域 `max_concurrent` 個 worker，同一 issue 不會重複派工，同一輪 assistant 的多個 tool call 平行執行
- **Skill**：讀取 `skills/*/SKILL.md`（YAML frontmatter + markdown），採漸進揭露，模型透過 `load_skill` 工具按需載入；也可依 issue label 自動附掛
- **MCP**：同時連接多個 MCP server（stdio 子程序或 streamable HTTP），工具以 `<server>__<tool>` 命名空間合併，支援 allow / deny 清單
- **無人值守**：以 label 驅動生命週期（`ai` → `ai:working` → `ai:done` / `ai:failed`），JSON 狀態檔避免重啟後重做，失敗自動留言，優雅關機會把佇列中的 issue 跑完

## 架構

```
cmd/gao                CLI: run / once / poll / skills / tools / prompt
internal/config        YAML 設定（${ENV} 展開、預設值、驗證）
internal/skill         SKILL.md 載入與索引
internal/mcp           多 MCP server 聚合成單一工具目錄
internal/llm           OpenAI-compatible tool-calling 迴圈（function calling、reasoning_effort、重試退避、平行 tool 執行）
internal/agent         組合 system prompt + skills + MCP tools，處理單一 issue
internal/github        極簡 REST client（輪詢、label、留言）
internal/state         原子寫入的 JSON 狀態檔
internal/orchestrator  輪詢、去重、worker pool、生命週期 label
skills/                內建範例 skill：issue-triage、bug-report-review、feature-request
```

資料流：

```
poller(repo) ─┐                        ┌─ load_skill ──► skills/*/SKILL.md
poller(repo) ─┼─► dispatch ─► worker ──┤
poller(repo) ─┘   (dedupe)   (N 併發)  └─ <server>__<tool> ──► MCP servers (GitHub …)
                                 │
                                 └─► LLM /chat/completions (tool loop) ─► labels + state.json
```

## 快速開始

需求：Go 1.27+、GitHub token、一個 OpenAI-compatible 端點（`OPENAI_API_KEY` / `OPENAI_BASE_URL`），以及 [github-mcp-server](https://github.com/github/github-mcp-server)（本機 binary 或使用 GitHub 託管的遠端 MCP）。

```bash
make build
cp config.example.yaml config.yaml      # 編輯 repos、trigger_label、model 等
export GITHUB_TOKEN=ghp_...
export OPENAI_API_KEY=sk-...
# 本機模型範例：export OPENAI_BASE_URL=http://localhost:11434/v1

./bin/gao skills                        # 列出載入的 skill
./bin/gao tools                         # 連線 MCP server 並列出工具
./bin/gao once -repo owner/name -issue 42   # 單一 issue 跑一次（測試用）
./bin/gao run                           # 常駐監看
```

在 GitHub 上為 issue 加上 `ai` label（可在設定改名），下一次輪詢就會被撿起。

## 設定

完整範例見 [`config.example.yaml`](config.example.yaml)。所有字串值支援 `${ENV_VAR}` 與 `${ENV_VAR:-default}` 展開。

| 區段 | 重點欄位 | 說明 |
|---|---|---|
| `llm` | `base_url`, `api_key`, `model`, `headers` | 任何 OpenAI-compatible 端點；`headers` 可加額外標頭 |
| `llm` | `max_tokens`, `max_tokens_param`, `effort`, `max_turns` | `max_tokens_param` 選 `max_completion_tokens`（OpenAI）或 `max_tokens`（舊伺服器、部分本機 runtime）；`effort` 非空時以 `reasoning_effort` 送出 |
| `github` | `repos`, `poll_interval`, `trigger_label`, `process_all` | `process_all: true` 時處理所有 open issue |
| `github` | `in_progress_label`, `done_label`, `failed_label` | 生命週期 label，設為空字串可停用 |
| `mcp.servers[]` | `command`+`args`+`env` 或 `url`+`headers` | 二擇一；`allow_tools` / `deny_tools` 過濾 |
| `skills` | `dir` | SKILL.md 根目錄 |
| `agent` | `max_concurrent`, `issue_timeout`, `state_file`, `system_prompt_file` | 併發數、單一 issue 逾時、狀態檔、自訂 system prompt |

### GitHub MCP 兩種接法

本機 stdio：

```yaml
mcp:
  servers:
    - name: github
      command: github-mcp-server
      args: ["stdio", "--toolsets", "issues,pull_requests,repos"]
      env: { GITHUB_PERSONAL_ACCESS_TOKEN: "${GITHUB_TOKEN}" }
```

GitHub 託管的遠端 MCP：

```yaml
mcp:
  servers:
    - name: github
      url: https://api.githubcopilot.com/mcp/
      headers: { Authorization: "Bearer ${GITHUB_TOKEN}" }
```

## 撰寫 Skill

```
skills/
└── my-skill/
    └── SKILL.md
```

```markdown
---
name: my-skill
description: 一句話說明何時該用這個 skill（會放進 system prompt）
labels: [bug]          # 選填：issue 有這些 label 時自動把 body 附進 prompt
---

# 給模型的完整步驟
...
```

system prompt 只放 name 與 description；模型需要時呼叫 `load_skill` 取得完整內容，避免 context 膨脹。

### 支援的 LLM 端點

只要實作 Chat Completions 的 function calling（`tools` / `tool_calls` / `role: tool`）即可。已知可用：OpenAI、Azure OpenAI（透過相容 gateway）、OpenRouter、vLLM、Ollama、LM Studio、DeepSeek。回應中的 `reasoning_content` 會原樣回傳給伺服器，符合 DeepSeek / vLLM 思考模式的要求。

本機模型請選 Ollama 標示支援 tools 的模型，例如 `qwen3:8b`、`llama3.1:8b`、`mistral-nemo`；`qwen2.5-coder` 系列會把工具呼叫寫成文字而非原生 `tool_calls`，不適用。可用 `gao ask -prompt "Use the load_skill tool to read issue-triage"` 快速確認，輸出的 `tool_calls` 應大於 0。

詳細的新增步驟、命名規則與常見陷阱見 [`docs/extending.md`](docs/extending.md)。

## 安全防護

- **Repo 鎖定**：任何 MCP 工具呼叫的 `owner` / `repo` 參數必須等於目前 issue 的 repo，否則拒絕並把原因回給模型。實測小模型會直接抄工具說明裡的範例參數（`octocat/Hello-World`），這個防護是必要的。`allow_other_repos: true` 可關閉。
- **Issue 鎖定**：`issue_scoped_tools` 列出的寫入工具，`issue_number` 必須等於目前處理的 issue。
- **必要工具**：`required_tools` 沒有成功呼叫時，模型會被提醒最多 `max_nudges` 次；仍未完成則整個 issue 判定失敗、貼 `ai:failed`，不會誤標完成。
- **工具黑白名單**：每個 MCP server 可設 `allow_tools` / `deny_tools`，建議把 merge、delete、force push 類工具列入 deny。

## 生命週期與重試

1. 輪詢到帶 `trigger_label` 且沒有 `done` / `failed` / `in_progress` label 的 open issue
2. 加上 `ai:working`，跑 agent
3. 成功：移除 `ai:working`、加 `ai:done`、寫入狀態檔
4. 失敗：移除 `ai:working`、加 `ai:failed`、留言錯誤摘要、寫入狀態檔
5. 重試：移除 `ai:failed` 再加回 `ai` 即可

`process_all` 模式下，issue 更新（`updated_at` 改變）後會再次處理。

## 部署

```bash
make docker
docker run -d --name gao \
  -e GITHUB_TOKEN -e OPENAI_API_KEY -e OPENAI_BASE_URL \
  -v $PWD/config.yaml:/app/config.yaml \
  -v gao-state:/app \
  gao:dev
```

映像檔以 distroless 為基底，內含 `gao` 與官方 `github-mcp-server` 兩個靜態 binary。

關機：第一次 SIGTERM 停止輪詢並等排隊與執行中的 issue 完成，第二次立即退出。

## 開發

```bash
make test     # go test -race ./...
make vet
```

測試不需要網路：LLM 迴圈用 httptest 假伺服器驗證，MCP hub 以 in-process 的 go-sdk server 驗證，GitHub client 用 httptest。

## License

MIT
