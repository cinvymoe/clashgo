# clashgo

用 Go 重写的 clashctl，管理 [mihomo](https://github.com/MetaCubeX/mihomo) 与 [zashboard](https://github.com/Zephyruso/zashboard)。

和 bash 版 `clashctl` **共用同一份数据面**（同一个 `.env`、同一个 `resources/`、同一个 `mihomo.service`），
所以两个 CLI 可以混着用，看到的是同一份状态。

## 为什么重写

bash 版的痛点是维护性，不是速度（`clashstatus` 只要 20ms）。具体：

| 症状 | bash 版 |
|---|---|
| 合并逻辑塞在 heredoc 里 | `_merge_config()` 85 行，其中 70 行是 yq 表达式 |
| YAML 全靠手写路径 | 30 处 `$BIN_YQ '.xxx // ""'` |
| 零测试 | 无任何 `*_test` |
| 错误即退出 | `_error_quit` 20 处，实现是 `exec $SHELL -i`（会吞掉调用者脚本） |
| 多 init 后端 | systemd / SysVinit / OpenRC / runit 各一套，共 1512 行 |

Go 版把这些收成类型化代码，语义由测试锁住。

## 实测差异

| 操作 | bash 版 | clashgo |
|---|---|---|
| 合并 config+mixin | 440ms（两次解析 51KB YAML） | **15ms** |
| 输出与 yq 逐字节一致 | — | ✅ golden test |
| 单测 | 0 | 96 个用例，15 个包全绿 |
| 依赖 `bin/yq` | 必须 | 不需要 |

## 安装

```bash
export PATH=$PATH:/usr/local/go/bin HOME=/root   # 本机 Go 不在 PATH，且需 HOME 才能定位模块缓存
cd /data/project/clashgo
go build -o clashgo .
install -m755 clashgo /usr/local/bin/clashgo     # 可选
```

## 命令

```
clashgo on            开启代理：启动内核 + 写 proxy.env + 维护 rc 托管块
clashgo off           关闭代理：停内核 + 清理 proxy.env 与托管块
clashgo status        内核状态 + API 探测 + 当前订阅
clashgo doctor        自检，一次列出所有问题（不遇到第一个就中止）
clashgo sub ls        列出订阅
clashgo sub add <url> 添加订阅（也支持本地路径 / file://）
clashgo sub use <id>  切换订阅（热重载生效）
clashgo sub update [id]   更新订阅，--all 更新全部
clashgo sub del <id>  删除订阅（正在使用的不允许删）
clashgo mixin         打开编辑器改 mixin → 校验 → 热重载
clashgo mixin -n      打印合并后的运行时配置（原名 merge --dry-run）
clashgo secret [x]    查看/修改控制密钥
clashgo tun [on|off]  TUN 开关（需重启内核）
clashgo proxy [--on|--off]  系统代理开关
clashgo ui            zashboard 地址
clashgo ui update     下载最新 zashboard 并原子替换 resources/dist
clashgo log [-f]      内核日志（透传 journalctl）
clashgo upgrade [-c alpha|release]  升级内核本体
clashgo merge -o f.yaml --check     合并并校验
clashgo restart       重启内核
clashgo version       版本信息
```

## 三个关键设计

### 1. 配置生效走热重载，不重启进程

`systemctl reload mihomo`（= `SIGHUP`）让内核**在进程内**重新解析 `runtime.yaml`。
实测：20MB 下载途中触发热重载不会断流，PID 不变，只有约 1s 抖动。
对比 `restart` 会让在途连接全部断开。

判定成功的条件是 **PID 未变 + API 可达**，不靠 grep 日志（同一秒内的旧日志会误判）。
热重载失败才回退 `restart`。

TUN 例外：设备只能在启动时创建，所以 `tun on/off` 仍然重启。

### 2. 写盘永远原子

合并结果先写 `runtime.yaml.new` → 内核 `-t` 校验 → 通过才 `mv -f` 覆盖。
坏 mixin 的后果是**报错 + 旧配置继续服务**，不会留下半截文件。
内容 md5 未变时完全不碰内核（重复更新订阅没有任何副作用）。

### 3. 不改 shell 函数

Go 改不了父 shell 的环境变量。这里不搞 `eval "$(clashgo env)"` 壳子，而是直接写配置：

- `~/.config/clashgo/proxy.env` 放 9 条 `export`
- `~/.bashrc` / `~/.zshrc` 里维护 `# clashgo START` … `# clashgo END` 托管块 source 它

**代价**：`clashgo on` 不影响当前已打开的 shell，需要 `source ~/.bashrc` 或新开终端。
（bash 版是立即生效的，这是唯一的行为倒退，命令输出里会提示。）

托管块与 `clashctl` 的块互不干扰，两者可以共存。

## 目录结构

```
clashgo/
├── main.go
├── cmd/              cobra 命令：只做参数解析与输出
└── internal/
    ├── app/          编排层：所有副作用都在这里（阶段二 daemon 直接复用）
    ├── env/          .env 解析、路径派生
    ├── cfg/          config + mixin → runtime 合并引擎（yaml.Node 节点级，保注释保顺序）
    ├── kernel/       内核生命周期：validate / reload / restart / apply
    ├── api/          Clash REST 客户端
    ├── sub/          订阅 CRUD + 下载 + subconverter
    ├── ui/           zashboard dist 下载与原子切换
    ├── state/        profiles.yaml 读写
    ├── livecfg/      只读解析已生效的 runtime.yaml
    ├── mixin/        mixin.yaml 读写（保注释）
    ├── shellcfg/     proxy.env + rc 托管块
    ├── netutil/      端口探测
    └── cli/          输出与配色
```

## 测试

```bash
export PATH=$PATH:/usr/local/go/bin HOME=/root
cd /data/project/clashgo && go test ./...
```

`internal/cfg` 的 golden test 会现场调用 `bin/yq` 算期望值再逐字节比对 —— 也就是说
合并引擎的正确性由 bash 版本身来背书。可用 `BIN_YQ` / `CLASHCTL_ROOT` 覆盖路径。

## 阶段二（未做）

`clashgo daemon` 挂 systemd：watch mixin 变化自动热重载、订阅定时更新（替代 cron）、
API 健康检查、本地 HTTP API。设计见 `docs/design.md` 第八节。
