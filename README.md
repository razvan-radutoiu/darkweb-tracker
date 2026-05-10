# DarkWeb Forums Tracker

> 监控 DarkWeb 论坛数据泄露，AI 自动评分，多渠道实时推送。

纯 Go 实现，单文件二进制，无需 Python/Node 运行时。支持 OpenAI / DeepSeek / Ollama 等任意 LLM，RSS 源通过远程 YAML 动态管理，域名迁移无需重新部署。

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
git clone https://github.com/your-username/darkweb-tracker
cd darkweb-tracker
go run ./cmd/tracker --once
```

程序会使用内置 seed 列表，自动做健康检查过滤死链，把新条目写入本地 `data_leaks.db`，生成日报到 `archive/` 目录。不配置推送渠道时静默运行，查看 `archive/` 下的 HTML 日报即可。

**加上 AI 分析和 Discord 推送：**

```bash
export DISCORD_WEBHOOK="https://discord.com/api/webhooks/..."
export DISCORD_ENABLED=true
export LLM_ENABLED=true
export LLM_API_KEY="sk-..."          # 或 DeepSeek / Ollama 的密钥
export LLM_MODEL="gpt-4o-mini"

go run ./cmd/tracker --once
```

---

## 工作原理

```
每次 Poll 循环（默认每 2 小时）
│
├─ 1. 加载 RSS 源
│      远程 sources.yaml → 本地 config.yaml → 内置 seed
│      → HEAD 健康检查，自动过滤无法访问的域名
│
├─ 2. 抓取 & 去重
│      feedparser 解析 RSS/Atom → 与 SQLite 比对链接
│      新条目写入 items 表
│
├─ 3. AI 分析（可选）
│      Worker Pool 并发调用 LLM（OpenAI-wire 协议）
│      严格 JSON Schema 输出 → 写入 analysis_results 表
│
│      score >= 8 AND is_urgent = true
│           └─ 立即推送紧急告警（红色 Embed）到所有渠道
│
├─ 4. 生成报告
│      日报：TOP 20 高价值条目摘要 + 完整列表
│      周报：每周五 15:00 北京时间自动生成
│      RSS XML：供 RSS 阅读器订阅
│
└─ 5. 推送报告摘要
       Discord / Telegram / 钉钉 / 飞书
```

### AI 评分说明

| 分数 | 含义 | 推送策略 |
|------|------|----------|
| 1–3 | 无效噪音（教程、一般讨论） | 静默存储 |
| 4–6 | 低中价值（旧泄露、小规模、待核实） | 正常推送 |
| 7–8 | 高价值（新鲜凭证/PII，>1 万条记录） | 正常推送 |
| **9–10** | **紧急（金融/医疗/政府，>10 万条）** | **立即单独告警** |

日报只展示当日评分最高的 TOP 20 条目。

---

## 安装

### 方式一：直接编译（推荐）

需要 Go 1.22+，无其他运行时依赖。

```bash
git clone https://github.com/your-username/darkweb-tracker
cd darkweb-tracker
go build -o tracker ./cmd/tracker
./tracker --version
```

### 方式二：Docker

```bash
docker pull ghcr.io/your-username/darkweb-tracker:latest

docker run -d \
  --name darkweb-tracker \
  -e DISCORD_WEBHOOK="https://discord.com/api/webhooks/..." \
  -e DISCORD_ENABLED=true \
  -e LLM_ENABLED=true \
  -e LLM_API_KEY="sk-..." \
  -v $(pwd)/data_leaks.db:/app/data_leaks.db \
  -v $(pwd)/archive:/app/archive \
  ghcr.io/your-username/darkweb-tracker:latest
```

### 方式三：GitHub Actions（无服务器运行）

详见下方 [GitHub Actions 完整配置](#github-actions-完整配置) 章节。

---

## GitHub Actions 完整配置

**零服务器成本，使用 GitHub 内置 AI，完全免费运行。**

### 第一步：Fork 仓库

```
https://github.com/your-username/darkweb-tracker
→ 点击右上角 Fork
```

Fork 完成后，`.github/workflows/` 中已包含以下工作流：

| 文件 | 触发方式 | 功能 |
|------|----------|------|
| `tracker.yml` | 每天北京时间 10:00 / 手动 | 核心：抓取 + AI 分析 + 提交 + 部署 |
| `weekly.yml` | 每周五 15:00 / 手动 | 生成并推送周报 |
| `export-training.yml` | 仅手动 | 导出 JSONL 训练数据集 |
| `setup-pages.yml` | 仅手动（首次） | 初始化 GitHub Pages |

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
https://your-username.github.io/darkweb-tracker/
```

---

### 第五步：触发首次运行

```
仓库 → Actions → DarkWeb Forums Tracker → Run workflow → Run workflow
```

或等待明天北京时间 10:00 自动触发。

首次运行完成后，检查：
- [ ] Actions 运行日志无报错
- [ ] 仓库出现 `data_leaks.db` 和 `archive/` 目录
- [ ] GitHub Pages 能访问（需等待 1-2 分钟）
- [ ] 推送渠道收到"服务启动"通知

---

### 工作流文件说明

#### tracker.yml — 核心工作流

```
触发 → 每天北京时间 10:00（cron: '0 10 * * *' timezone: Asia/Shanghai）
     + 手动触发（支持 debug / skip_ai / health_check 开关）

Job: track
  ├─ Checkout（fetch-depth: 0，完整历史）
  ├─ git pull（拉取上次运行的 db 文件）
  ├─ Setup Go 1.22 + 依赖缓存
  ├─ Build（注入版本号）
  ├─ Run tracker --once
  │    ├─ 加载远程 sources.yaml + 健康检查
  │    ├─ 抓取 RSS → 去重 → 写 SQLite
  │    ├─ GitHub Models AI 分析（gpt-4o-mini）
  │    ├─ 紧急条目立即推送（score ≥ 8）
  │    └─ 生成日报 HTML + RSS XML
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
| `health_check` | true | 对 RSS 源做健康检查 |

---

## 配置

所有配置项均支持两种方式，**环境变量优先级更高**：

```
环境变量  >  config.yaml  >  默认值
```

### RSS 源管理

这是本工具最关键的配置。Dark web 论坛域名平均每 3–6 个月迁移一次，建议使用远程源动态管理。

**三层加载机制（优先级由高到低）：**

```
远程 sources.yaml（GitHub）
    ↓ 若远程不可达，降级
本地 config.yaml 的 data_sources 块
    ↓ 合并补充
内置 seed 列表（代码内，永不为空）
    ↓ 全部合并后
HEAD 健康检查（并发探活，自动剔除死链）
```

**config.yaml 配置：**

```yaml
sources:
  # 远程源 URL，支持两种格式：
  # 格式 A：GitHub 简写（自动展开为 raw URL）
  remote_url: "your-username/darkweb-tracker/main/sources.yaml"
  # 格式 B：完整 HTTPS URL
  # remote_url: "https://raw.githubusercontent.com/your-username/darkweb-tracker/main/sources.yaml"

  # 同名源冲突时，是否让本地配置优先（默认远程优先）
  local_overrides_remote: false

  # 启动时并发 HEAD 探活，自动跳过无响应的 URL（推荐开启）
  health_check: true

  # 禁用内置 seed（若完全自己管理源列表）
  disable_builtin_seeds: false
```

**远程 sources.yaml 格式：**

```yaml
version: "1"
updated: "2026-05-11"

sources:
  - name: "leakbase"
    rss_url: "https://leakbase.la/forums/-/index.rss"
    enabled: true
    tags: ["database", "credentials", "pii"]
    note: ""

  - name: "xforums.st"
    rss_url: "https://xforums.st/forums/-/index.rss"
    enabled: false                 # 暂时宕机，设 false 跳过
    note: "2026-05 超时，待确认新域名"
```

> **维护建议：** Fork 本项目，修改根目录的 `sources.yaml`，推送后所有运行实例下次启动自动获取最新源。无需重新部署二进制。

**内置 seed（当前验证可用）：**

| 源名称 | 当前状态 |
|--------|----------|
| gerki | ✅ |
| blackbones | ✅ |
| hard-tm | ✅ |
| ascarding | ✅ |
| mipped | ✅ |
| leakbase | ✅ |
| dublikat | ✅ |
| cardforum | ✅ |
| ipbmafia | ✅ |
| xforums.st | ❌ 默认禁用（超时） |
| htdark | ❌ 默认禁用（403） |
| niflheim | ❌ 默认禁用（403） |
| sinister | ❌ 默认禁用（DNS 失败） |

---

### AI 分析

支持任何实现了 OpenAI Chat Completions API 的服务：官方 OpenAI、DeepSeek、Ollama 本地部署、LiteLLM 网关等。

**config.yaml：**

```yaml
llm:
  enabled: false
  provider: "openai"       # 仅用于日志标识
  base_url: ""             # 留空 = 官方 OpenAI
  api_key: ""
  model: "gpt-4o-mini"
  rpm: 60                  # 每分钟最大请求数
  rpm_burst: 5             # 令牌桶突发量
  workers: 3               # 并发分析 goroutine 数
  urgent_score_threshold: 8  # >= 此分立即推送告警
  daily_top_n: 20          # 日报展示 TOP N 条目
  skip_analyzed_items: true  # 重启后跳过已分析条目（节省费用）
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
LLM_RPM=30               # DeepSeek 免费层限制

# Ollama（本地，零费用）
LLM_ENABLED=true
LLM_BASE_URL=http://localhost:11434/v1
LLM_API_KEY=ollama        # 随意填写
LLM_MODEL=llama3.2
LLM_RPM=600               # 本地无限速
```

**AI 输出结构（每条分析结果）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `score` | int 1–10 | 数据泄露价值评分 |
| `category` | string | 主分类标签（见下表） |
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
    send_normal_msg: true     # 每条新发现推送
    send_daily_report: false  # 日报推送（内容较长）
    send_weekly_report: true  # 周报推送
```

消息颜色含义：
- 🟢 绿色：服务启动通知
- 🔴 红色：紧急告警（score ≥ 8）
- 🟣 紫色：日报 / 周报
- 🎨 随机色：普通新条目

#### Telegram

```yaml
push:
  telegram:
    token: "7xxxxxxx:AAF..."   # BotFather 获取
    chat_id: "-100xxxxxxx"     # 群组或频道 ID
    enabled: true
```

#### 钉钉

```yaml
push:
  dingtalk:
    webhook: "https://oapi.dingtalk.com/robot/send?access_token=..."
    secret_key: "SEC..."       # 安全设置 → 加签
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
```

代理对 RSS 抓取和所有推送渠道均生效。访问 dark web 论坛的 clearnet 镜像通常不需要代理；若需要访问 `.onion` 地址，需要配置 Tor SOCKS5 代理（`socks5://127.0.0.1:9050`）。

---

### 报告与调度

```yaml
night_sleep:
  enabled: true    # 北京时间 00:00–07:00 自动跳过轮询
  start_hour: 0
  end_hour: 7

daily_report:
  enabled: true    # 每次轮询结束后生成当日报告到 archive/

weekly_report:
  enabled: true
  push_enabled: true
  push_time: "15:00"   # 北京时间
  push_day: 5          # 5 = 周五

interval: "2h"         # 轮询间隔，支持 1h / 30m / 2h 等格式
```

报告文件存储结构：

```
archive/
├── 2026-05-11/
│   ├── Daily_2026-05-11.md    # Markdown 版日报
│   └── Daily_2026-05-11.html  # 带样式的 HTML 日报
└── Weekly_2026-05-05_2026-05-11.md

rss/
├── daily_rss_2026-05-11.xml
├── latest_daily_rss.xml       # 始终指向最新日报（推荐订阅此链接）
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
| `SOURCES_HEALTH_CHECK` | `true` | 启动时健康探活 |
| `SOURCES_NO_SEEDS` | `false` | 禁用内置 seed |
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
| **调度** | | |
| `NIGHT_SLEEP_ENABLED` | `true` | 夜间休眠开关 |
| `DAILY_REPORT_ENABLED` | `true` | 生成日报 |
| `WEEKLY_REPORT_ENABLED` | `true` | 生成周报 |
| `WEEKLY_REPORT_PUSH_ENABLED` | `true` | 推送周报 |
| `POLL_INTERVAL` | `2h` | 轮询间隔 |
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

# 自定义路径
./tracker \
  --config /etc/tracker/config.yaml \
  --db /var/data/leaks.db \
  --archive /var/reports \
  --once
```

### Docker

```bash
# 基础运行（自动使用内置 seed）
docker run --rm \
  -e DISCORD_WEBHOOK="https://discord.com/api/webhooks/..." \
  -e DISCORD_ENABLED=true \
  -v $(pwd)/data_leaks.db:/app/data_leaks.db \
  -v $(pwd)/archive:/app/archive \
  ghcr.io/your-username/darkweb-tracker:latest --once

# 完整配置（挂载自定义 config.yaml）
docker run -d \
  --name tracker \
  --restart unless-stopped \
  -v $(pwd)/config.yaml:/app/config.yaml:ro \
  -v $(pwd)/data_leaks.db:/app/data_leaks.db \
  -v $(pwd)/archive:/app/archive \
  -v $(pwd)/rss:/app/rss \
  ghcr.io/your-username/darkweb-tracker:latest
```

**docker-compose.yml：**

```yaml
services:
  tracker:
    image: ghcr.io/your-username/darkweb-tracker:latest
    restart: unless-stopped
    volumes:
      - ./config.yaml:/app/config.yaml:ro
      - ./data_leaks.db:/app/data_leaks.db
      - ./archive:/app/archive
      - ./rss:/app/rss
    environment:
      - DISCORD_WEBHOOK=${DISCORD_WEBHOOK}
      - DISCORD_ENABLED=true
      - LLM_ENABLED=${LLM_ENABLED:-false}
      - LLM_API_KEY=${LLM_API_KEY}
      - LLM_MODEL=${LLM_MODEL:-gpt-4o-mini}
      - SOURCES_REMOTE_URL=${SOURCES_REMOTE_URL}
```

### GitHub Actions

项目内置 `.github/workflows/tracker.yml`，Fork 后配置以下 Secrets 即可：

**必填 Secrets：**
- `DISCORD_WEBHOOK`

**可选 Secrets：**
- `LLM_API_KEY` — 开启 AI 分析
- `SOURCES_REMOTE_URL` — 使用远程源列表

默认执行计划：北京时间每天 10:00 运行一次（`--once` 模式），结果 commit 回仓库，部署在 GitHub Pages 可直接访问 HTML 报告。

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
content        TEXT
download_links TEXT
site_name      TEXT NOT NULL
created_at     DATETIME
ai_score       INTEGER DEFAULT 0       -- AI 评分快速过滤
ai_category    TEXT DEFAULT ''
ai_urgent      INTEGER DEFAULT 0       -- 1 = 紧急
urgent_notified INTEGER DEFAULT 0      -- 1 = 已推送告警
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

# 查看导出结果
head -n 1 training_data.jsonl | python3 -m json.tool
```

每行一个 JSON 对象，字段全部使用 `snake_case`（Python 兼容）：

```json
{
  "item_id": 42,
  "title": "Selling fresh 500k combo list from banking portal",
  "content": "...",
  "site_name": "leakbase",
  "score": 9,
  "category": "credential_leak",
  "tags": ["email", "password", "banking", "fresh"],
  "summary": "出售来自某银行门户网站的 50 万条新鲜账号密码组合，数据采集于本月。",
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

**Python 加载示例：**

```python
import pandas as pd

df = pd.read_json("training_data.jsonl", lines=True)

# 高价值数据（score >= 7）
high_value = df[df["score"] >= 7]

# 按类别分布统计
print(df["category"].value_counts())

# 训练集分割
from sklearn.model_selection import train_test_split
train, test = train_test_split(df, test_size=0.2, stratify=df["category"])
```

---

## RSS 源维护

### 为什么需要维护源列表？

Dark web 论坛域名极其不稳定：
- 平均迁移周期：**3–6 个月**
- 执法行动期间可能**每周更换**
- 迁移通常通过 Telegram 频道或 Dread 论坛公告

### 推荐工作流

```
每月检查一次：

1. 查看程序日志中的健康检查失败记录
   grep "health check failed" tracker.log

2. 在 fastfire/deepdarkCTI 的 forum.md 查找新地址
   https://github.com/fastfire/deepdarkCTI/blob/main/forum.md

3. 更新你 fork 的 sources.yaml：
   - 将迁移的旧域名 enabled 改为 false，note 写明原因
   - 添加新域名条目（enabled: false，先验证再启用）

4. git push → 所有实例下次启动自动生效
```

### 手动验证新 URL

```bash
# 测试 RSS URL 是否有效
curl -sI "https://newdomain.example/forums/-/index.rss" | head -5

# 测试是否返回 RSS 内容
curl -s "https://newdomain.example/forums/-/index.rss" | head -20

# 运行单次并开 debug 查看源加载情况
SOURCES_REMOTE_URL="" ./tracker --once --debug 2>&1 | grep -E "source|health"
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
│   ├── sources/
│   │   └── loader.go        # 三层源加载：远程 → 本地 → seed；健康检查
│   │
│   ├── feed/
│   │   └── fetcher.go       # RSS/Atom 抓取、内容清洗、下载链接提取
│   │
│   ├── storage/
│   │   ├── storage.go       # SQLite 基础 CRUD（items 表）
│   │   └── analysis.go      # AI 结果存储、TopN 查询、训练数据导出
│   │
│   ├── analyzer/
│   │   └── analyzer.go      # LLM 调用、结构化输出、Worker Pool、限速
│   │
│   ├── notify/
│   │   └── notify.go        # 4 渠道推送：Discord/Telegram/钉钉/飞书
│   │
│   ├── report/
│   │   └── report.go        # Markdown + HTML 日报/周报；RSS XML 生成
│   │
│   └── scheduler/
│       └── scheduler.go     # 主循环、夜眠、AI 集成、紧急告警、周报调度
│
├── config.yaml              # 默认配置模板（含完整注释）
├── sources.yaml             # 社区 RSS 源列表（Fork 后维护此文件）
├── Dockerfile               # 多阶段构建，最终镜像 ~20MB
└── .github/workflows/
    └── tracker.yml          # GitHub Actions 定时运行
```

---

## 常见问题

**Q：不配置 LLM_API_KEY 也能运行吗？**

可以。AI 分析是可选功能，不配置时程序正常抓取、去重、生成报告，只是没有评分和紧急告警。日报会展示所有条目而不是 TOP 20。

---

**Q：日报/周报生成了，但 Discord 没有收到？**

检查 `DISCORD_SEND_DAILY` 和 `DISCORD_SEND_WEEKLY` 是否设为 `true`。默认日报不推送（内容较长），只有周报推送。

---

**Q：health_check 报告某个源失败，但我知道它是活的？**

某些论坛对 `HEAD` 请求返回 403，但对 `GET` 正常响应（例如 htdark）。这种情况下健康检查会误报。

解决方案：关闭健康检查，手动管理 enabled 状态：

```bash
SOURCES_HEALTH_CHECK=false ./tracker --once
```

---

**Q：如何只监控某几个源？**

方式一：在 `config.yaml` 的 `data_sources` 中只保留需要的条目，其余删除。

方式二：通过环境变量禁用不需要的源：

```bash
DATASOURCE_CARDFORUM=false DATASOURCE_IPBMAFIA=false ./tracker --once
```

方式三：设置 `disable_builtin_seeds: true`，只使用你在 `data_sources` 中显式配置的源。

---

**Q：SQLite 数据库文件会无限增长吗？**

RSS 条目会持续累积，建议定期归档或清理旧数据。可以用以下 SQL 手动清理 90 天前的记录：

```bash
sqlite3 data_leaks.db "DELETE FROM items WHERE created_at < date('now', '-90 days'); VACUUM;"
```

---

**Q：如何让同一台机器监控不同的源列表？**

为每个实例使用独立的数据库和配置文件：

```bash
# 实例 A：只监控金融类论坛
./tracker --config config-financial.yaml --db financial.db --once

# 实例 B：监控所有论坛
./tracker --config config-all.yaml --db all.db --once
```
