# clashgo 设计文档（阶段一：Go CLI）

> 状态：待确认
> 目标：用 Go 重写 clashctl，与现有 bash 版并存；阶段一交付可独立运行的 CLI，阶段二挂 systemd 常驻服务。

## 一、要解决的问题

bash 版 `clashctl` 的真实痛点是**维护性**，不是运行速度（实测：`clashstatus` 20ms、`clashsub ls` 13ms，已经很快）。具体负担：

| 症状 | 量化 |
|---|---|
| 配置合并逻辑塞在 heredoc 里 | `_merge_config()` 85 行，其中 70 行是 yq/jq 表达式字符串 |
| YAML 操作全靠手写路径表达式 | 30 处 `$BIN_YQ '.xxx // ""'` |
| CLI 表面重复实现 | 1512 行 bash + 87 行 fish 补全，无共同真相 |
| 零测试 | 无任何 `*_test` / spec |
| 错误处理即退出 | `_error_quit` 20 处，实现是 `exec $SHELL -i`（会吞掉调用者的脚本） |
| 状态分散 | `.env` + `mixin.yaml` + `profiles.yaml` + `runtime.yaml` + `profiles.log` |
| 多 init 后端 | systemd / SysVinit / OpenRC / runit 各一套 |

所以 design goal 是：**类型安全、可单测、单一真相源**，而不是"更快"。

## 二、非目标（阶段一明确不做）

- 不做 Web 面板（zashboard 继续作为 UI）
- 不做 OpenRC / SysVinit / runit 后端，只支持 systemd，其余检测到直接报错退出
- 不重写 subconverter，继续调用 `bin/subconverter`
- 不删除 bash 版，`/root/clashctl` 继续可用

## 三、关键决策

### D1：并存，二进制叫 `clashgo`

bash 版继续留在 `/data/project/clashctl`，`.bashrc` / `.zshrc` 里的 `source` 行不动。新二进制独立名 `clashgo`，等稳定后再讨论接管。

### D2：共用同一个数据面

`clashgo` 读同一个 `.env` → 同一个 `CLASH_BASE_DIR=/data/project/clashctl` → 同一份 `resources/`、同一个 `mihomo.service`。

理由：mihomo 只能有一个实例占 7890/9090/TUN；两套数据目录会产生两份 `runtime.yaml`、两份 `selected` 状态，必然打架。

因此**两个 CLI 可以混用**：今天用 `clashctl`，明天用 `clashgo`，看到的是同一份配置。

并发保护：对 `resources/` 加 `flock`，两个 CLI 同时触发合并时串行化。

### D3：不引入 shell 函数，只写 shell 配置文件

`clashon` / `clashoff` 需要向**当前 shell** `export http_proxy` 等 9 个变量。Go 进程改不了父 shell 环境，所以有两种做法：

- ~~shell 函数壳子 + `eval "$(clashgo env)"`~~（用户否决）
- **写配置文件**（采纳）

具体做法：`clashgo on` 把 export 行写进托管块：

```
# ~/.bashrc / ~/.zshrc
# clashgo START
[ -f ~/.config/clashgo/proxy.env ] && . ~/.config/clashgo/proxy.env
# clashgo END
```

`~/.config/clashgo/proxy.env` 内容由 `clashgo` 直接生成：

```sh
export http_proxy=http://127.0.0.1:7890
export HTTP_PROXY=$http_proxy
...
```

`clashgo off` 删除该文件，新 shell 即无代理。

**已知代价**：`clashgo on` 不会立刻影响当前 shell，需要 `source ~/.bashrc` 或开新窗口。bash 版是立即生效的。这一点会写在命令输出提示里。

### D4：合并引擎用 yaml.v3 的节点级 API，不用结构体

必须保留 YAML 注释和 key 顺序（`runtime.yaml` 里有 10 行注释是从 `mixin.yaml` 带过来的，`#authentication:` 这种被注释掉的 key 也要保留）。所以合并操作在 `yaml.Node` 上做，不做 marshal/unmarshal 到 struct。

### D5：语义正确性用 golden test 保证

把 yq 版对真实文件的输出固化成 golden 文件，Go 版逐字节比对。差一个字节就说明翻译错了。

## 四、合并引擎的语义契约

从 `scripts/cmd/clashctl.sh` 的 yq 表达式 + 实测 yq 行为反推，必须精确复刻：

### 4.1 深合并 `$config * $mixin`

- map：递归合并，mixin 的 key 覆盖 config
- **数组和标量：mixin 整体替换 config**（实测 `[1,2,3] * [4,5]` → `[4,5]`，不是追加）
- mixin 先 `del(._custom)`（`_custom` 是 clashgo 自己的元数据区，不写进 runtime）

### 4.2 列表重建（因为 4.1 会把数组整个替换掉）

```
rules        = mixin.rules.prefix        + config.rules        + mixin.rules.suffix
proxies      = mixin.proxies.prefix      + override(config.proxies,      mixin.proxies.override)      + mixin.proxies.suffix
proxy-groups = mixin.proxy-groups.prefix + override(config.proxy-groups, mixin.proxy-groups.override) + mixin.proxy-groups.suffix
```

`override(list, overrides)`：按 `.name` 匹配，命中替换，未命中保留原样（实测 `($o[] | select(.name == $i.name)) // $i`）。同名多条 override 时取第一条。

### 4.3 inject

```
for each group g in .proxy-groups:
    extra = mixin.proxy-groups.inject[g.name] // []
    g.proxies = unique(g.proxies + extra)
```

`unique` **保序去重**（实测 `["b","a","b","c"]` → `["b","a","c"]`）。

### 4.4 校验与写盘

1. 合并结果写到 `runtime.yaml.new`
2. `mihomo -d <resources> -f runtime.yaml.new -t` 校验
3. 失败 → 删 `.new`、报错、**旧 `runtime.yaml` 原样保留**（原子性）
4. 成功 → `mv -f` 原子替换
5. 内容未变（md5 相同）→ 不做任何事，不触发 reload

### 4.5 生效策略（沿用刚做的热重载）

- `systemctl reload mihomo`（= `SIGHUP`，进程内重解析，不断连）
- 判定：PID 不变 + API `/version` 可达
- 失败 → 回退 `systemctl restart mihomo`

## 五、目录结构

```
/data/project/clashgo
├── go.mod                     module clashgo
├── main.go                    子命令注册
├── cmd/                       cobra 命令
│   ├── on.go off.go status.go
│   ├── sub.go                 sub ls/add/del/use/update
│   ├── mixin.go secret.go log.go tun.go upgrade.go
│   ├── ui.go                  zashboard 管理
│   ├── merge.go               merge --dry-run：只算不写，用于 diff 和测试
│   └── doctor.go              自检
├── internal/
│   ├── env/                   .env 解析、路径推导
│   ├── cfg/                   config+mixin → runtime 合并引擎
│   │   ├── merge.go
│   │   ├── merge_test.go      纯语义单测
│   │   └── testdata/          golden 文件
│   ├── kernel/                mihomo 生命周期：start/stop/reload/restart/validate/upgrade
│   ├── api/                   Clash REST 客户端
│   ├── sub/                   订阅 CRUD + 下载 + subconverter
│   ├── ui/                    zashboard dist 下载 / 校验 / 切换
│   ├── state/                 profiles.yaml 读写
│   └── shellcfg/              写托管块到 rc 文件 + proxy.env
└── docs/design.md
```

## 六、命令表（阶段一）

| 命令 | 行为 | 与 bash 版差异 |
|---|---|---|
| `clashgo on` | 启动内核 + 写 proxy.env + 托管块 | 不立即影响当前 shell（见 D3） |
| `clashgo off` | 停内核 + 删 proxy.env | 同上 |
| `clashgo status` | 内核状态 + API 探测 + 当前订阅 | 信息更全 |
| `clashgo sub ls/add/del/use/update` | 订阅管理 | 同 |
| `clashgo mixin` | 打开编辑器 → 合并 → 校验 → 热重载 | 同 |
| `clashgo secret [x]` | 查看/修改密钥 → 热重载 | 同 |
| `clashgo ui` | 打印面板地址 | 同 |
| `clashgo ui update` | 下载 zashboard 最新 dist → 校验 → 原子替换 `resources/dist` | **新增**（bash 版靠安装包里的 zip） |
| `clashgo log [-f]` | 内核日志 | 同 |
| `clashgo tun on/off` | TUN 开关 | 同 |
| `clashgo upgrade [channel]` | 内核升级 | 同 |
| `clashgo merge --dry-run` | 输出合并结果到 stdout，不写盘 | **新增**（测试入口） |
| `clashgo doctor` | 自检：内核版本、API 可达、端口冲突、配置与订阅一致性 | **新增** |

## 七、验收标准

1. `clashgo merge --dry-run` 对真实 `config.yaml` + `mixin.yaml` 的输出与 yq 版逐字节一致
2. 合并耗时 < 50ms（对比 yq 版 440ms）
3. 破坏性测试：坏 mixin → 报错、`runtime.yaml` 不变、内核继续服务
4. `clashgo sub update` 后 PID 不变、规则生效
5. `clashgo on` / `off` 后开新 shell，`env | grep -i proxy` 结果正确
6. 全程不依赖 `bin/yq`
7. 单测覆盖 4.1–4.3 全部语义分支

## 八、阶段二（本次不做，仅记录）

`clashgo daemon`，systemd 常驻：

- 文件 watch：`mixin.yaml` / `config.yaml` 变化 → 自动合并 + 热重载
- 定时任务：订阅按 interval 自动更新（替代 cron）
- 健康检查：API 无响应 → 自动重启内核 + 记录
- 本地 HTTP API：给以后的面板/脚本用
