# DarkWeb Forums Tracker

> 监控 DarkWeb 论坛数据泄露，AI 自动评分，多渠道实时推送。

纯 Go 实现，单文件二进制，无需 Python/Node 运行时。支持 OpenAI / DeepSeek / Ollama 等任意 LLM，RSS 源和过滤规则均通过外置 YAML 动态管理，迭代规则无需重新编译。

---

## 目录

- [快速开始](#快速开始)
- [工作原理](#工作原理)
- [安装](#安装)
- [GitHub Actions 完整配置](#github-actions-完整配置)
  - [第一步：Fork 仓库](#第一步fork-仓库)
  - [第二步：配置 Secrets](#第二步配置-secrets)
  - [第三步：开启 GitHub Models AI](#第三步开启-github-models-ai)
  - [第四步：配置 GitHub Pages](#第四步配置-github-pages)
  - [第五步：触发首次运行](#第五步触发首次运行)
  - [工作流文件说明](#工作流文件说明)
- [配置](#配置)
  - [RSS 源管理](#rss-源管理)
  - [过滤规则（filter_rules.yaml）](#过滤规则filter_rulesyaml)
  - [正文全文抓取](#正文全文抓取)
  - [AI 分析](#ai-分析)
  - [推送渠道](#推送渠道)
  - [代理](#代理)
  - [报告与调度](#报告与调度)
  - [完整环境变量表](#完整环境变量表)
- [运行方式](#运行方式)
  - [本地运行](#本地运行)
  - [Docker](#docker)
- [数据结构](#数据结构)
- [训练数据导出](#训练数据导出)
- [RSS 源维护](#rss-源维护)
- [项目结构](#项目结构)
- [常见问题](#常见问题)

---

## 快速开始

**最简体验（无需任何配置）：**

```bash
git clone https://github.com/bambooqj/darkweb-tracker
cd darkweb-tracker
go run ./cmd/tracker --once
```

程序会使用内置 seed 列表，把新条目写入本地 `data_leaks.db`，生成日报到 `archive/` 目录。不配置推送渠道时静默运行，查看 `archive/` 下的 HTML 日报即可。

**加上 AI 分析和 Discord 推送：**

```bash
export DISCORD_WEBHOOK="https://discord.com/api/webhooks/..."
export DISCORD_ENABLED=true
export LLM_ENABLED=true
export LLM_API_KEY="sk-..."          # 或 DeepSeek / Ollama 的密钥
export LLM_MODEL="gpt-4o-mini"

go run ./cmd/tracker --once
```

**开启正文全文抓取（大幅提升 AI 分析质量）：**

```bash
export FETCH_FULL_CONTENT=true
go run ./cmd/tracker --once
```

---

## 工作原理

```
每次 Poll 循环（默认每 2 小时）
│
├─ 1. 加载 RSS 源
│      远程 sources.yaml → 本地 config.yaml → 内置 seed
│      → 自动探活，过滤无响应域名
│
├─ 2. 抓取 & 去重
│      feedparser 解析 RSS/Atom → 与 SQLite 比对链接
│      新条目写入 items 表（content = RSS teaser）
│
├─ 3. 正文抓取（可选，FETCH_FULL_CONTENT=true）
│      GET 帖子链接 → go-readability 提取主体 → html-to-markdown
│      → 写入 items.full_content（Markdown 格式）
│
├─ 4. 标题预过滤（filter_rules.yaml 驱动）
│      命中 named_targets → 强制进入分析
│      命中 skip_patterns → 存 DB 但跳过分析和推送
│
├─ 5. AI 分析（可选）
│      Worker Pool 并发调用 LLM（OpenAI-wire 协议）
│      优先使用 full_content，fallback 到 RSS teaser
│      严格 JSON Schema 输出 → 写入 analysis_results 表
│
│      score >= 8 AND is_urgent = true
│           └─ 立即推送紧急告警（红色 Embed）到所有渠道
│
├─ 6. 生成报告
│      日报：TOP 20 高价值条目摘要 + 完整列表
│      周报：每周五 15:00 北京时间自动生成
│      RSS XML：供 RSS 阅读器订阅
│
├─ 7. 推送报告摘要
│      Discord / Telegram / 钉钉 / 飞书
│
└─ 8. 数据清理（RETENTION_DAYS，默认 90 天）
       自动删除过期条目，回收磁盘空间
```

### AI 评分说明

| 分数 | 含义 | 推送策略 |
|------|------|----------|
| 1–3 | 无效噪音（教程、供应商广告、一般讨论） | 静默存储 |
| 4 | Combo 列表聚合（非新鲜泄露，上限） | 静默存储 |
| 5–6 | 低中价值（旧泄露、小规模、待核实） | 正常推送 |
| 7–8 | 高价值（新鲜凭证/PII，>1 万条记录） | 正常推送 |
| **9–10** | **紧急（金融/医疗/政府，>10 万条）** | **立即单独告警** |

日报只展示当日评分最高的 TOP 20 条目。

---

## 安装

### 方式一：直接编译（推荐）

需要 Go 1.22+，无其他运行时依赖。

```bash
git clone https://github.com/bambooqj/darkweb-tracker
cd darkweb-tracker
go build -o tracker ./cmd/tracker
./tracker --version
```

### 方式二：Docker

```bash
docker pull ghcr.io/bambooqj/darkweb-tracker:latest

docker run -d \
  --name darkweb-tracker \
  -e DISCORD_WEBHOOK="https://discord.com/api/webhooks/..." \
  -e DISCORD_ENABLED=true \
  -e LLM_ENABLED=true \
  -e LLM_API_KEY="sk-..." \
  -e FETCH_FULL_CONTENT=true \
  -v $(pwd)/data_leaks.db:/app/data_leaks.db \
  -v $(pwd)/archive:/app/archive \
  -v $(pwd)/filter_rules.yaml:/app/filter_rules.yaml:ro \
  ghcr.io/bambooqj/darkweb-tracker:latest
```

### 方式三：GitHub Actions（无服务器运行）

详见下方 [GitHub Actions 完整配置](#github-actions-完整配置) 章节。

---

## GitHub Actions 完整配置

**零服务器成本，使用 GitHub 内置 AI，完全免费运行。**

### 第一步：Fork 仓库

```
https://github.com/bambooqj/darkweb-tracker
→ 点击右上角 Fork
```

Fork 完成后，`.github/workflows/` 中已包含以下工作流：

| 文件 | 触发方式 | 功能 |
|------|----------|------|
| `tracker.yml` | **手动触发**（默认暂停） | 核心：抓取 + AI 分析 + 提交 + 部署 |
| `weekly.yml` | **手动触发**（默认暂停） | 生成并推送周报 |
| `build.yml` | 每次代码提交自动触发 | 编译检查，验证代码可构建 |
| `export-training.yml` | 仅手动 | 导出 JSONL 训练数据集 |
| `setup-pages.yml` | 仅手动（首次） | 初始化 GitHub Pages |

> **重要**：`tracker.yml` 和 `weekly.yml` 默认处于**暂停**状态（schedule 已注释）。配置好 Secrets 后，在工作流文件中取消注释 `schedule:` 部分启用定时运行，或直接手动触发测试。

---

### 第二步：配置 Secrets

进入你 Fork 的仓库：**Settings → Secrets and variables → Actions → New repository secret**

#### 必填（推送渠道，至少配置一个）

| Secret 名称 | 说明 | 获取方式 |
|-------------|------|----------|
| `DISCORD_WEBHOOK` | Discord 频道 Webhook | Discord 频道设置 → 整合 → Webhook |
| `TELEGRAM_TOKEN` | Telegram Bot Token | 向 [@BotFather](https://t.me/BotFather) 发送 `/newbot` |
| `TELEGRAM_CHAT_ID` | 频道/群组 ID | 将 Bot 加入群组后，访问 `api.telegram.org/bot<TOKEN>/getUpdates` |
| `DINGTALK_WEBHOOK` | 钉钉机器人 Webhook | 钉钉群 → 机器人管理 → 添加机器人 |
| `DINGTALK_SECRET` | 钉钉机器人签名密钥 | 同上，安全设置选"加签" |
| `FEISHU_WEBHOOK` | 飞书机器人 Webhook | 飞书群 → 机器人 → 添加 → 自定义机器人 |

> 不需要的渠道不填即可，程序会自动检测并跳过。

#### 可选（使用外部 LLM 时才需要）

| Secret 名称 | 说明 |
|-------------|------|
| `LLM_API_KEY` | 外部 LLM 密钥（使用 GitHub Models 时**不需要**填，见下方） |

---

### 第三步：开启 GitHub Models AI

**GitHub Models 是 GitHub 内置的 AI 推理服务，对公共仓库免费，无需填写任何 API Key。**

工作流已预配置好，`tracker.yml` 中的关键部分：

```yaml
permissions:
  models: read          # ← 开启 GitHub Models API 权限

env:
  LLM_ENABLED:   "true"
  LLM_BASE_URL:  "https://models.github.ai/inference"
  LLM_API_KEY:   ${{ secrets.GITHUB_TOKEN }}   # ← 使用内置 token，无需额外配置
  LLM_MODEL:     "openai/gpt-4o-mini"          # ← 免费高效模型
```

#### 可用模型

| 模型名称（LLM_MODEL 填写值） | 特点 |
|------------------------------|------|
| `openai/gpt-4o-mini` | **推荐**，快速、免费、适合分类任务 |
| `openai/gpt-4o` | 更高精度，消耗配额更多 |
| `meta/llama-3.1-70b-instruct` | 开源模型，无版权问题 |
| `mistral-ai/mistral-large` | 适合多语言内容 |

> 完整模型列表：[github.com/marketplace/models](https://github.com/marketplace/models)

#### 免费层限制

| 限制项 | 值 |
|--------|-----|
| 每分钟请求数 (RPM) | ~10（工作流已配置为 10） |
| 每天请求数 | 视账号类型，通常数百次 |
| 上下文长度 | 与模型相同（gpt-4o-mini: 128k） |
| 费用 | **$0**（公共仓库 / GitHub Free 账号） |

---

### 第四步：配置 GitHub Pages

1. 进入仓库：**Settings → Pages**
2. Source 选择：**GitHub Actions**（不是 Deploy from branch）
3. 保存

然后手动触发一次初始化：**Actions → Setup GitHub Pages → Run workflow**

之后每次主工作流运行，HTML 报告会自动部署到：
```
https://bambooqj.github.io/darkweb-tracker/
```

---

### 第五步：触发首次运行

```
仓库 → Actions → DarkWeb Forums Tracker → Run workflow → Run workflow
```

首次运行完成后，检查：
- [ ] Actions 运行日志无报错
- [ ] 仓库出现 `data_leaks.db` 和 `archive/` 目录
- [ ] GitHub Pages 能访问（需等待 1-2 分钟）
- [ ] 推送渠道收到"服务启动"通知

---

### 工作流文件说明

#### build.yml — CI 编译检查

```
触发 → 任意分支 push（排除 .md / sources.yaml / filter_rules.yaml / 生成文件）
     + Pull Request

步骤：
  go build ./...           ← 验证所有包可编译
  go vet ./...             ← 静态分析
  交叉编译 linux/amd64     ← 验证跨平台构建
```

提交代码后可在 Actions 页面查看构建状态。

#### tracker.yml — 核心工作流

```
触发 → 手动触发（配置后可取消注释 schedule 启用定时）

Job: track
  ├─ Checkout（fetch-depth: 0，完整历史）
  ├─ git pull（拉取上次运行的 db 文件）
  ├─ 安装并启动 Tor（可选，--skip_tor 跳过）
  ├─ Setup Go 1.22 + 依赖缓存
  ├─ Build（注入版本号）
  ├─ Run tracker --once
  │    ├─ 加载远程 sources.yaml + filter_rules.yaml
  │    ├─ 抓取 RSS → 去重 → 写 SQLite
  │    ├─ 正文全文抓取（可选）
  │    ├─ GitHub Models AI 分析（gpt-4o-mini）
  │    ├─ 紧急条目立即推送（score ≥ 8）
  │    ├─ 生成日报 HTML + RSS XML
  │    └─ 自动清理过期数据（90 天）
  ├─ git-auto-commit（提交 db / archive / rss / index.html）
  └─ upload-pages-artifact（打包报告）

Job: deploy-pages（依赖 track）
  └─ deploy-pages（部署到 GitHub Pages）

Job: cleanup（依赖 track）
  └─ 清理 14 天前的运行记录
```

#### 并发控制

```yaml
concurrency:
  group: darkweb-tracker
  cancel-in-progress: false   # 不取消正在运行的任务
```

确保同一时间只有一个实例在操作 SQLite 数据库，避免数据损坏。

#### 手动触发参数

在 **Actions → DarkWeb Forums Tracker → Run workflow** 时可以调整：

| 参数 | 默认 | 说明 |
|------|------|------|
| `debug` | false | 开启详细日志（排查问题用） |
| `skip_ai` | false | 跳过 AI 分析（节省配额） |
| `skip_tor` | false | 跳过 Tor 代理（直连抓取，速度更快） |

---

## 配置

所有配置项均支持两种方式，**环境变量优先级更高**：

```
环境变量  >  config.yaml  >  默认值
```

### RSS 源管理

**三层加载机制（优先级由高到低）：**

```
远程 sources.yaml（GitHub）
    ↓ 若远程不可达，降级
本地 config.yaml 的 data_sources 块
    ↓ 合并补充
内置 seed 列表（代码内，永不为空）
    ↓
自动发现（deepdarkCTI / ransomware.live / ransomwatch / ransomlook）
```

**config.yaml 配置：**

```yaml
sources:
  remote_url: "bambooqj/darkweb-tracker/main/sources.yaml"
  local_overrides_remote: false
  health_check: false          # GitHub Actions runner 网络与论坛连通性不同，建议关闭
  disable_builtin_seeds: false
  disable_auto_discover: false # 自动从 deepdarkCTI 发现新源
```

**远程 sources.yaml 格式：**

```yaml
version: "1"
updated: "2026-05-11"

sources:
  - name: "breachforums-rs"
    rss_url: "https://breachforums.rs/syndication.php?limit=50"
    enabled: true
    tags: ["database", "credentials", "tier1"]

  - name: "xforums.st"
    rss_url: "https://xforums.st/forums/-/index.rss"
    enabled: false
    note: "2026-05 超时，待确认新域名"
```

> **维护建议：** 修改根目录的 `sources.yaml` 后推送，所有运行实例下次启动自动获取最新源，无需重新部署。

---

### 过滤规则（filter_rules.yaml）

**过滤规则完全外置为 YAML 文件，修改后重启生效，无需重新编译。**

```yaml
# filter_rules.yaml — 三部分结构

skip_patterns:      # 命中任意 keyword → 存 DB 但跳过分析和推送
  - { keyword: "combo list",   reason: "email combo aggregation" }
  - { keyword: "whatsapp:",    reason: "vendor contact post" }
  # ... 更多模式

named_targets:      # 命中任意条目 → 强制进入分析（优先于 skip_patterns）
  - "hospital"
  - "bank"
  - "government"
  # ... 更多高价值目标

rules:
  vendor_patterns:  # 匹配任意条目 → 规则引擎直接返回 score=2
    - "whatsapp:"
    - "combo mixed email"

  tier1_sites:      # 一级论坛，评分 +1
    - "breachforum"
    - "leakbase"

  data_types:       # 数据类型关键词 → 影响评分加成
    email:    ["email", "@gmail", "@yahoo"]
    password: ["password", "passwd", "pwd"]
    # ...
```

**关键设计：**
- `skip_patterns` 与 `named_targets` 均为小写子串匹配，不区分大小写
- `named_targets` 优先级高于 `skip_patterns`，确保命名受害者不被误过滤
- `combo` 数据类型命中后，规则引擎将评分**上限钳制为 4**，防止 combo 列表误报为高危泄露
- 供应商广告模式（WhatsApp 号码、护照售卖等）由 `vendor_patterns` 统一拦截，直接返回 score=2

如需自定义规则，直接编辑 `filter_rules.yaml`；文件不存在时程序使用内置默认规则。

指定自定义规则文件路径：

```bash
./tracker --rules /path/to/my_rules.yaml --once
```

---

### 正文全文抓取

默认情况下，程序只使用 RSS feed 提供的 teaser 片段（通常 200-500 字）作为 AI 分析的输入。开启全文抓取后，程序会额外 GET 帖子原始链接，通过 [go-readability](https://codeberg.org/readeck/go-readability) 提取正文，再用 [html-to-markdown](https://github.com/JohannesKaufmann/html-to-markdown) 转为干净 Markdown，作为 AI 分析的主要输入。

**效果对比：**

| | RSS Teaser | 全文抓取 |
|---|---|---|
| 内容长度 | ~200 字 | 完整帖子正文 |
| 信息完整性 | 截断，常有"登录查看" | 完整内容（公开可见部分） |
| AI 评分准确性 | 受限 | 显著提升 |
| 额外网络请求 | 无 | 每个新条目一次 GET |

**config.yaml 配置：**

```yaml
fetch:
  full_content: false           # FETCH_FULL_CONTENT — 总开关
  content_timeout: 15s          # FETCH_CONTENT_TIMEOUT — 单页超时
  content_max_bytes: 524288     # FETCH_CONTENT_MAX_BYTES — 最大读取 512KB
```

**环境变量：**

```bash
FETCH_FULL_CONTENT=true
FETCH_CONTENT_TIMEOUT=20s
FETCH_CONTENT_MAX_BYTES=524288
```

> **注意**：全文抓取通过与 RSS 抓取相同的 HTTP client 发出，代理/Tor 设置同样生效。暗网论坛 clearnet 镜像通常可以直接访问。

---

### AI 分析

支持任何实现了 OpenAI Chat Completions API 的服务：官方 OpenAI、DeepSeek、Ollama 本地部署、LiteLLM 网关等。

**config.yaml：**

```yaml
llm:
  enabled: false
  provider: "openai"
  base_url: ""             # 留空 = 官方 OpenAI
  api_key: ""
  model: "gpt-4o-mini"
  rpm: 60
  rpm_burst: 5
  workers: 3
  urgent_score_threshold: 8
  notify_min_score: 5      # >= 此分才推送（低于此分仅存 DB）
  daily_top_n: 20
  skip_analyzed_items: true
  max_input_chars: 0       # 0 = 不限制（全文内容较长时可设 30000）

  # 备用提供商链（主提供商失败时自动切换）
  fallback_providers:
    - name: "deepseek"
      base_url: "https://api.deepseek.com/v1"
      api_key: ""
      model: "deepseek-chat"
      rpm: 30
```

**各提供商配置示例：**

```bash
# OpenAI
LLM_ENABLED=true
LLM_API_KEY=sk-proj-...
LLM_MODEL=gpt-4o-mini

# DeepSeek（更便宜，推荐）
LLM_ENABLED=true
LLM_BASE_URL=https://api.deepseek.com/v1
LLM_API_KEY=sk-...
LLM_MODEL=deepseek-chat
LLM_RPM=30

# Ollama（本地，零费用）
LLM_ENABLED=true
LLM_BASE_URL=http://localhost:11434/v1
LLM_API_KEY=ollama
LLM_MODEL=llama3.2
LLM_RPM=600
```

**AI 输出结构（每条分析结果）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `score` | int 1–10 | 数据泄露价值评分 |
| `category` | string | 主分类标签 |
| `tags` | []string | 次级多标签 |
| `summary` | string | 1–2 句中文摘要 |
| `affected_targets` | []string | 受影响组织/国家/平台 |
| `estimated_records` | int | 估计泄露记录数（-1=未知） |
| `data_types` | []string | 数据字段类型 |
| `is_urgent` | bool | 是否需立即告警 |
| `confidence_level` | string | high / medium / low |
| `reasoning` | string | 评分理由（训练数据用） |

**category 枚举值：**

`credential_leak` · `database_dump` · `financial_data` · `personal_info` · `malware_distribution` · `access_sale` · `data_trade` · `vulnerability_info` · `general_discussion` · `other`

---

### 推送渠道

所有渠道可同时开启，紧急告警（score ≥ 8）会以单独消息推送到所有已启用渠道。

#### Discord

```yaml
push:
  discord:
    webhook: "https://discord.com/api/webhooks/..."
    enabled: true
    send_normal_msg: true
    send_daily_report: false
    send_weekly_report: true
```

消息颜色：🟢 启动 · 🔴 紧急告警 · 🟣 日报/周报 · 🎨 随机色普通条目

#### Telegram

```yaml
push:
  telegram:
    token: "7xxxxxxx:AAF..."
    chat_id: "-100xxxxxxx"
    enabled: true
```

#### 钉钉

```yaml
push:
  dingtalk:
    webhook: "https://oapi.dingtalk.com/robot/send?access_token=..."
    secret_key: "SEC..."
    enabled: true
```

#### 飞书

```yaml
push:
  feishu:
    webhook: "https://open.feishu.cn/open-apis/bot/v2/hook/..."
    enabled: true
```

---

### 代理

```yaml
proxy:
  enabled: false
  http: "http://127.0.0.1:7890"
  https: "http://127.0.0.1:7890"
  no_proxy: "localhost,127.0.0.1"
  tor_socks: ""    # TOR_SOCKS — 如 "socks5://127.0.0.1:9050"
```

代理优先级：**Tor SOCKS5 > HTTP/HTTPS 代理 > 直连**。推送渠道（Discord/Telegram 等）使用独立 HTTP client，不经过 Tor，避免 clearnet API 连通性问题。

---

### 报告与调度

```yaml
night_sleep:
  enabled: true
  start_hour: 0    # 北京时间 00:00
  end_hour: 7      # 北京时间 07:00

daily_report:
  enabled: true

weekly_report:
  enabled: true
  push_enabled: true
  push_time: "15:00"
  push_day: 5       # 5 = 周五

interval: "2h"

retention_days: 90  # RETENTION_DAYS — 0=永久保留，默认 90 天自动清理
```

报告文件存储结构：

```
archive/
├── 2026-05-11/
│   ├── Daily_2026-05-11.md
│   └── Daily_2026-05-11.html
└── Weekly_2026-05-05_2026-05-11.md

rss/
├── daily_rss_2026-05-11.xml
├── latest_daily_rss.xml        # 推荐订阅此链接
├── weekly_rss_2026-05-05_2026-05-11.xml
└── latest_weekly_rss.xml
```

---

### 完整环境变量表

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| **源管理** | | |
| `SOURCES_REMOTE_URL` | `""` | 远程 sources.yaml URL 或 GitHub 简写 |
| `SOURCES_LOCAL_OVERRIDES` | `false` | 本地源优先于远程 |
| `SOURCES_HEALTH_CHECK` | `false` | 启动时健康探活 |
| `SOURCES_NO_SEEDS` | `false` | 禁用内置 seed |
| `SOURCES_NO_AUTODISCOVER` | `false` | 禁用自动发现 |
| **正文抓取** | | |
| `FETCH_FULL_CONTENT` | `false` | 开启全文抓取总开关 |
| `FETCH_CONTENT_TIMEOUT` | `15s` | 单页抓取超时 |
| `FETCH_CONTENT_MAX_BYTES` | `524288` | 最大读取字节数（512KB） |
| **AI 分析** | | |
| `LLM_ENABLED` | `false` | 总开关 |
| `LLM_PROVIDER` | `openai` | 提供商标识（仅日志用） |
| `LLM_BASE_URL` | `""` | API 端点（空 = 官方 OpenAI）|
| `LLM_API_KEY` | `""` | API 密钥（必填） |
| `LLM_MODEL` | `gpt-4o-mini` | 模型名称 |
| `LLM_RPM` | `60` | 每分钟请求限制 |
| `LLM_RPM_BURST` | `5` | 令牌桶突发量 |
| `LLM_WORKERS` | `3` | 并发分析 goroutine 数 |
| `LLM_URGENT_THRESHOLD` | `8` | 紧急告警分数阈值 |
| `LLM_NOTIFY_MIN_SCORE` | `5` | 最低推送分数 |
| `LLM_MAX_INPUT_CHARS` | `0` | 输入截断（0=不限） |
| `LLM_DAILY_TOP_N` | `20` | 日报 TOP N 条目数 |
| `LLM_SKIP_ANALYZED` | `true` | 跳过已分析条目 |
| **Discord** | | |
| `DISCORD_WEBHOOK` | `""` | Webhook URL |
| `DISCORD_ENABLED` | `false` | 开关 |
| `DISCORD_SEND_NORMAL` | `true` | 推送普通新条目 |
| `DISCORD_SEND_DAILY` | `false` | 推送日报 |
| `DISCORD_SEND_WEEKLY` | `true` | 推送周报 |
| **Telegram** | | |
| `TELEGRAM_TOKEN` | `""` | Bot Token |
| `TELEGRAM_CHAT_ID` | `""` | 群组/频道 ID |
| `TELEGRAM_ENABLED` | `false` | 开关 |
| **钉钉** | | |
| `DINGTALK_WEBHOOK` | `""` | Webhook URL |
| `DINGTALK_SECRET` | `""` | 签名密钥 |
| `DINGTALK_ENABLED` | `false` | 开关 |
| **飞书** | | |
| `FEISHU_WEBHOOK` | `""` | Webhook URL |
| `FEISHU_ENABLED` | `false` | 开关 |
| **代理** | | |
| `PROXY_ENABLED` | `false` | 代理总开关 |
| `HTTP_PROXY` | `""` | HTTP 代理地址 |
| `HTTPS_PROXY` | `""` | HTTPS 代理地址 |
| `TOR_SOCKS` | `""` | Tor SOCKS5 地址，如 `socks5://127.0.0.1:9050` |
| **调度** | | |
| `NIGHT_SLEEP_ENABLED` | `true` | 夜间休眠开关 |
| `DAILY_REPORT_ENABLED` | `true` | 生成日报 |
| `WEEKLY_REPORT_ENABLED` | `true` | 生成周报 |
| `WEEKLY_REPORT_PUSH_ENABLED` | `true` | 推送周报 |
| `POLL_INTERVAL` | `2h` | 轮询间隔 |
| `RETENTION_DAYS` | `90` | 数据保留天数（0=永久） |
| `MAX_ITEM_AGE_DAYS` | `7` | 超过此天数的帖子跳过分析 |
| **单数据源开关** | | |
| `DATASOURCE_<KEY>` | — | 如 `DATASOURCE_GERKI=false` 禁用单个源 |

---

## 运行方式

### 本地运行

```bash
# 单次执行（适合测试 / 定时任务）
./tracker --once

# 循环模式（适合长期运行）
./tracker

# 开启调试日志
./tracker --once --debug

# 指定自定义规则文件
./tracker --rules my_rules.yaml --once

# 自定义路径
./tracker \
  --config /etc/tracker/config.yaml \
  --rules /etc/tracker/filter_rules.yaml \
  --db /var/data/leaks.db \
  --archive /var/reports \
  --once
```

### Docker

```bash
# 基础运行
docker run --rm \
  -e DISCORD_WEBHOOK="https://discord.com/api/webhooks/..." \
  -e DISCORD_ENABLED=true \
  -e FETCH_FULL_CONTENT=true \
  -v $(pwd)/data_leaks.db:/app/data_leaks.db \
  -v $(pwd)/archive:/app/archive \
  -v $(pwd)/filter_rules.yaml:/app/filter_rules.yaml:ro \
  ghcr.io/bambooqj/darkweb-tracker:latest --once

# 完整配置
docker run -d \
  --name tracker \
  --restart unless-stopped \
  -v $(pwd)/config.yaml:/app/config.yaml:ro \
  -v $(pwd)/filter_rules.yaml:/app/filter_rules.yaml:ro \
  -v $(pwd)/data_leaks.db:/app/data_leaks.db \
  -v $(pwd)/archive:/app/archive \
  -v $(pwd)/rss:/app/rss \
  ghcr.io/bambooqj/darkweb-tracker:latest
```

**docker-compose.yml：**

```yaml
services:
  tracker:
    image: ghcr.io/bambooqj/darkweb-tracker:latest
    restart: unless-stopped
    volumes:
      - ./config.yaml:/app/config.yaml:ro
      - ./filter_rules.yaml:/app/filter_rules.yaml:ro
      - ./data_leaks.db:/app/data_leaks.db
      - ./archive:/app/archive
      - ./rss:/app/rss
    environment:
      - DISCORD_WEBHOOK=${DISCORD_WEBHOOK}
      - DISCORD_ENABLED=true
      - LLM_ENABLED=${LLM_ENABLED:-false}
      - LLM_API_KEY=${LLM_API_KEY}
      - LLM_MODEL=${LLM_MODEL:-gpt-4o-mini}
      - FETCH_FULL_CONTENT=${FETCH_FULL_CONTENT:-false}
      - RETENTION_DAYS=90
```

---

## 数据结构

### SQLite 表结构

**`items` 表** — 原始抓取数据：

```sql
id             INTEGER PRIMARY KEY
title          TEXT NOT NULL
link           TEXT NOT NULL UNIQUE    -- 去重键
pub_date       TEXT
author         TEXT
category       TEXT
content        TEXT                    -- RSS teaser（始终可用）
full_content   TEXT DEFAULT ''         -- 全文 Markdown（FETCH_FULL_CONTENT=true 时填充）
download_links TEXT
site_name      TEXT NOT NULL
created_at     DATETIME
ai_score       INTEGER DEFAULT 0       -- AI 评分快速过滤
ai_category    TEXT DEFAULT ''
ai_urgent      INTEGER DEFAULT 0       -- 1 = 紧急
urgent_notified INTEGER DEFAULT 0     -- 1 = 已推送告警
```

**`analysis_results` 表** — AI 分析结果：

```sql
id               INTEGER PRIMARY KEY
item_id          INTEGER → items.id   -- 级联删除
score            INTEGER (1–10)
category         TEXT
tags             TEXT (JSON 数组)
summary          TEXT
affected_targets TEXT (JSON 数组)
estimated_records INTEGER
data_types       TEXT (JSON 数组)
is_urgent        INTEGER (0/1)
confidence_level TEXT (high/medium/low)
reasoning        TEXT
model_used       TEXT
analyzed_at      DATETIME
```

---

## 训练数据导出

导出所有已完成 AI 分析的条目为 JSONL 格式，用于训练分类模型：

```bash
# 导出最近 30 天（默认）
./tracker --export-training

# 指定时间范围
./tracker --export-training --since 2026-01-01 --until 2026-04-30 --out q1_2026.jsonl
```

每行一个 JSON 对象，字段全部使用 `snake_case`（Python 兼容）：

```json
{
  "item_id": 42,
  "title": "Selling fresh 500k combo list from banking portal",
  "content": "RSS teaser...",
  "full_content": "完整正文 Markdown（如已抓取）",
  "site_name": "leakbase",
  "score": 9,
  "category": "credential_leak",
  "tags": ["email", "password", "banking", "fresh"],
  "summary": "出售来自某银行门户网站的 50 万条新鲜账号密码组合。",
  "affected_targets": ["Banking Portal"],
  "estimated_records": 500000,
  "data_types": ["email", "password"],
  "is_urgent": true,
  "confidence_level": "high",
  "reasoning": "Score 9: banking credentials, 500k records, described as fresh.",
  "model_used": "deepseek-chat",
  "created_at": "2026-05-10 14:32:00",
  "analyzed_at": "2026-05-10 14:32:05"
}
```

---

## RSS 源维护

### 为什么需要维护源列表？

Dark web 论坛域名极其不稳定，平均迁移周期 3–6 个月。

### 推荐工作流

```
每月检查一次：

1. 查看日志中的 fetch failed 记录
   grep "fetch failed" tracker.log

2. 在 fastfire/deepdarkCTI 查找最新状态
   https://github.com/fastfire/deepdarkCTI/blob/main/forum.md

3. 更新 sources.yaml：
   - 旧域名设 enabled: false，写明原因
   - 新域名设 enabled: false，验证后再启用

4. git push → 所有实例下次启动自动生效
```

### 常见论坛 RSS 路径规律

| 论坛软件 | RSS 路径 |
|----------|----------|
| XenForo | `/forums/-/index.rss` |
| MyBB | `/syndication.php?limit=50` |
| vBulletin | `/external.php?type=RSS2` |
| phpBB | `/feed.php` |
| IPB | `/rss/1-temy.xml/` |

---

## 项目结构

```
darkweb-tracker/
│
├── cmd/tracker/
│   └── main.go              # 入口：flag 解析、依赖注入、信号处理
│
├── internal/
│   ├── config/
│   │   └── config.go        # YAML 加载 + 环境变量覆盖（分层合并）
│   │
│   ├── rules/
│   │   └── rules.go         # 过滤规则结构体 + YAML 加载器（含内置默认值）
│   │
│   ├── sources/
│   │   └── loader.go        # 多层源加载：远程 → 本地 → seed → 自动发现
│   │
│   ├── feed/
│   │   └── fetcher.go       # RSS/Atom 抓取；全文抓取（readability + html-to-markdown）
│   │
│   ├── storage/
│   │   ├── storage.go       # SQLite CRUD（items 表，含 full_content 列）
│   │   └── analysis.go      # AI 结果存储、TopN 查询、训练数据导出、Prune TTL
│   │
│   ├── analyzer/
│   │   ├── analyzer.go      # LLM 调用、流式 API、Worker Pool、限速、重试
│   │   ├── rules.go         # 规则引擎：纯确定性评分，从 FilterRules 读取所有配置
│   │   └── hybrid.go        # HybridAnalyzer：多提供商链式 fallback
│   │
│   ├── filter/
│   │   └── titlefilter.go   # 标题预过滤（消费 FilterRules，零硬编码）
│   │
│   ├── notify/
│   │   └── notify.go        # 4 渠道推送：Discord / Telegram / 钉钉 / 飞书
│   │
│   ├── report/
│   │   └── report.go        # HTML/MD 日报周报（html/template）；RSS（gorilla/feeds）
│   │
│   ├── scheduler/
│   │   └── scheduler.go     # 主循环、pipeline、紧急告警、报告调度、TTL 清理
│   │
│   └── web/
│       ├── server.go        # 内置 HTTP 服务（Telegram Login Widget 认证）
│       ├── auth.go          # Session 管理
│       └── api.go           # REST API：/api/items /api/stats
│
├── config.yaml              # 主配置模板（含完整注释）
├── filter_rules.yaml        # 过滤规则（可单独迭代，无需重编译）
├── sources.yaml             # RSS 源列表（Fork 后在此维护）
├── Dockerfile               # 多阶段构建，最终镜像 ~20MB
└── .github/workflows/
    ├── build.yml            # CI：每次提交自动编译检查
    ├── tracker.yml          # 核心工作流（默认暂停，配置后启用）
    ├── weekly.yml           # 周报工作流（默认暂停）
    ├── export-training.yml  # 训练数据导出（仅手动）
    └── setup-pages.yml      # GitHub Pages 初始化（首次运行一次）
```

---

## 常见问题

**Q：不配置 LLM_API_KEY 也能运行吗？**

可以。不配置 LLM 时，程序使用内置规则引擎（`rules.go`）进行确定性评分：根据记录数量、数据类型关键词、站点分级等规则估算分数，无需网络调用，完全免费。规则引擎的模式同样从 `filter_rules.yaml` 加载，可以自定义。

---

**Q：如何自定义过滤规则而不重新编译？**

直接编辑 `filter_rules.yaml`，重启程序即可。支持：
- 添加/删除 `skip_patterns` 中的关键词
- 添加新的 `named_targets`（高价值目标白名单）
- 调整 `rules.vendor_patterns`（供应商垃圾帖识别）
- 修改 `rules.data_types`（数据类型关键词）

文件不存在时程序自动使用内置默认规则，不影响启动。

---

**Q：日报/周报生成了，但 Discord 没有收到？**

检查 `DISCORD_SEND_DAILY` 和 `DISCORD_SEND_WEEKLY` 是否设为 `true`。默认日报不推送（内容较长），只有周报推送。

---

**Q：health_check 报告某个源失败，但我知道它是活的？**

某些论坛对 `HEAD` 请求返回 403，但对 `GET` 正常响应。建议关闭健康检查，手动管理源状态：

```bash
SOURCES_HEALTH_CHECK=false ./tracker --once
```

---

**Q：SQLite 数据库文件会无限增长吗？**

不会。程序内置 TTL 清理机制，每次 poll 结束后自动删除超过 `retention_days`（默认 90 天）的旧记录并压缩磁盘空间。

调整保留天数：

```bash
RETENTION_DAYS=30 ./tracker --once   # 只保留最近 30 天
RETENTION_DAYS=0  ./tracker --once   # 永久保留（不清理）
```

---

**Q：如何让同一台机器监控不同的源列表？**

```bash
# 实例 A：金融类论坛 + 专用规则
./tracker --config config-financial.yaml --rules rules-financial.yaml --db financial.db --once

# 实例 B：全部源 + 默认规则
./tracker --config config-all.yaml --db all.db --once
```

---

**Q：全文抓取会拖慢速度吗？**

每个新帖子会额外发出一次 HTTP GET（默认 15 秒超时）。抓取与分析是并行流水线，通常不会显著增加总耗时。若遇到慢速源：

```bash
FETCH_CONTENT_TIMEOUT=8s ./tracker --once   # 缩短超时
FETCH_FULL_CONTENT=false ./tracker --once   # 或直接关闭
```
