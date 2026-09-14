# 新增 MCP server 與 skill

兩者都不需要改 Go 程式碼。MCP server 加在設定檔，skill 放一個目錄。這是刻意的設計：`internal/mcp` 與 `internal/skill` 在啟動時掃描，其餘程式碼只看聚合後的結果。

---

## 新增一個 MCP server

### 1. 加進設定檔

`config.yaml` 的 `mcp.servers` 是一個清單，每個項目必須二擇一：`command`（stdio 子程序）或 `url`（streamable HTTP）。同時設或都不設都會在啟動時被 `Validate` 擋下。

stdio 子程序：

```yaml
mcp:
  servers:
    - name: filesystem
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/srv/workspace"]
      env:
        SOME_TOKEN: ${SOME_TOKEN}
      deny_tools: [write_file, move_file]
```

streamable HTTP：

```yaml
    - name: sentry
      url: https://mcp.sentry.dev/mcp
      headers:
        Authorization: "Bearer ${SENTRY_TOKEN}"
      allow_tools: [find_errors, get_issue_details]
```

欄位說明：

| 欄位 | 說明 |
|---|---|
| `name` | 工具命名空間前綴，必填且不可重複 |
| `command` / `args` / `env` | stdio 模式。`env` 疊加在目前環境變數上，不是取代 |
| `url` / `headers` | HTTP 模式。`headers` 每次請求都會帶上 |
| `allow_tools` | 白名單。設了就只曝露清單內的工具 |
| `deny_tools` | 黑名單。在白名單之後套用 |

所有字串值支援 `${VAR}` 與 `${VAR:-預設值}` 展開，token 不要寫死在檔案裡。

### 2. 確認工具有出現

```bash
./bin/gao tools
```

這個指令只連線、列出工具、關閉，不會碰 LLM 也不會動 GitHub。看到工具名稱就代表連線與過濾都正確。

### 3. 注意命名規則

工具在模型眼中的名字是 `<server>__<tool>`，例如 `sentry__find_errors`。規則在 `internal/mcp.QualifiedName`：

- 非英數與 `_`、`-` 的字元會換成 `_`
- 超過 64 字元會被截斷，因為那是 Anthropic 與 OpenAI 的工具名稱上限
- 截斷後撞名的工具會被跳過，並留一則 warn log

**容易踩的坑**：`allow_tools` 與 `deny_tools` 用的是**原始工具名**（`find_errors`），但 `agent.required_tools` 與 `agent.issue_scoped_tools` 用的是**命名空間後的名字**（`sentry__find_errors`）。前者在過濾階段、後者在執行階段，所以取名的時機不同。

### 4. 如果新工具會寫東西，要更新防護

`internal/agent` 的 guard 會檢查每次工具呼叫：

- `owner` / `repo` 參數必須等於目前 issue 的 repo，否則拒絕。`agent.allow_other_repos: true` 可以關掉。
- `agent.issue_scoped_tools` 清單裡的工具，`issue_number` 必須等於目前 issue。

預設清單只涵蓋 GitHub MCP 的三個寫入工具。新增其他會寫入 issue 的工具時，要自己加進去：

```yaml
agent:
  issue_scoped_tools:
    - github__add_issue_comment
    - github__issue_write
    - github__sub_issue_write
    - linear__create_comment      # 新增的
```

guard 只認得 `owner`、`repo`、`issue_number` 這三個參數名。如果新 MCP server 用別的命名（例如 `repository`、`project_id`），guard 對它無效，得用 `deny_tools` 或在 `internal/agent/agent.go` 的 `guard` 加對應。

### 5. Context 成本

每個工具定義都會佔 token，而且每一輪都重送。GitHub MCP 一家就 22 個工具、數千 token。實測小模型會因此看不到完整清單而產生幻覺。

所以新增 server 時，優先用 `allow_tools` 只留真正需要的，不要整包接上。長遠解法是按需載入，追蹤在 #3。

---

## 新增一個 skill

### 1. 建目錄與檔案

```
skills/
└── security-review/
    └── SKILL.md
```

檔名必須是 `SKILL.md`，而且要在 `skills/` 的直接子目錄下。`internal/skill.Load` 只掃一層，不遞迴。

### 2. 寫 frontmatter 與內容

```markdown
---
name: security-review
description: >-
  Review an issue that reports a security concern. Check the affected code
  for the reported class of problem and summarise severity and blast radius.
  Attaches automatically to issues labelled "security".
labels: [security]
---

# Security review

## Steps

1. ...
```

| 欄位 | 必填 | 說明 |
|---|---|---|
| `name` | 否 | 省略時用目錄名。不可與其他 skill 重複，重複會在啟動時報錯 |
| `description` | 實質必填 | 這是**唯一**會進 system prompt 的內容，模型靠它決定要不要載入 |
| `labels` | 否 | issue 帶到其中任一 label 時，整份 body 自動附進 prompt |

沒有 frontmatter 也能讀，整份檔案會被當成 body，但這樣模型不知道它存在，等於沒用。

**容易踩的坑**：`description` 如果包含冒號後接空白，YAML 會解析失敗。用 `>-` 折行語法最安全，這是先前三個內建 skill 都改用它的原因。

### 3. 兩種載入方式

- **漸進揭露**：system prompt 只列 name 與 description。模型判斷需要時呼叫 `load_skill` 工具取得 body。適合大部分情況，省 context。
- **label 自動附掛**：`labels` 命中時，body 直接寫進 issue prompt，模型不必呼叫工具。適合一定要遵守的流程，代價是 token 固定支出。

兩者可以並存，同一個 skill 被自動附掛後，system prompt 會提醒模型不必重複載入。

### 4. 驗證

```bash
./bin/gao skills      # 確認有被讀到、name 與 labels 正確
./bin/gao prompt      # 看 description 在 system prompt 裡長什麼樣
./bin/gao ask -prompt "Use load_skill to read security-review, then summarise its steps in one sentence."
```

`gao ask` 會實際呼叫模型但不碰 GitHub，是驗證 skill 內容能不能被理解的最快方法。

### 5. 寫給模型看，不是寫給人看

內建三個 skill 的共通結構，建議沿用：

- 開頭一句 **Goal**，說清楚什麼算完成
- **Steps** 用編號，每步一個動作
- 要輸出留言時，直接給**格式範本**，不要只描述
- 結尾一段 **Do not**，明確列出禁止事項

最後一段特別重要。實測顯示模型會做出沒被禁止但不該做的事，例如擅自關閉 issue。

---

## 檢查清單

新增 MCP server：

- [ ] `config.yaml` 加設定，`command` 或 `url` 只設一個
- [ ] token 用 `${VAR}` 不寫死
- [ ] `allow_tools` 只留需要的工具
- [ ] `gao tools` 看得到，名稱沒被截斷撞名
- [ ] 有寫入能力的工具加進 `agent.issue_scoped_tools`
- [ ] 確認 guard 認得它的參數命名

新增 skill：

- [ ] `skills/<name>/SKILL.md`
- [ ] `description` 用 `>-` 折行，說清楚何時該用
- [ ] 需要自動附掛就設 `labels`
- [ ] `gao skills` 看得到
- [ ] `gao ask` 驗證模型能讀懂
- [ ] 有 **Do not** 段落
