# 达梦 → MongoDB 数据迁移工具 · 现场部署与操作手册

> **这份文档是给现场实施人员的。**
> 你只需要两个可执行文件和一个配置文件，**不需要 Go 环境、不需要联网下载任何依赖**。
> 编译方法、代码结构等开发内容见 [DEVELOP.md](DEVELOP.md)，现场无需阅读。

把达梦（DM8）里的存量数据搬到 MongoDB。两个程序互相独立，可以单独运行、单独重跑。

| 程序 | 源表（达梦） | 目标集合（MongoDB） | 数据量级 | 预期耗时 |
| --- | --- | --- | --- | --- |
| `migrate_user` | `DLPDATA.did_system_sync_user` | `identity.user` | 约 40 万条 | 1～3 分钟 |
| `migrate_org` | `DLPDATA.did_system_sync_org` | `identity.organization` | 约 10 万条 | 1 分钟以内 |

---

## 目录

- [一、交付物清单](#一交付物清单)
- [二、开工前要凑齐的信息](#二开工前要凑齐的信息)
- [三、开工前环境检查](#三开工前环境检查)
- [四、部署](#四部署)
- [五、修改 .env（现场必做）](#五修改-env现场必做)
- [六、执行顺序（四步走）](#六执行顺序四步走)
- [七、进度、日志与中断](#七进度日志与中断)
- [八、参数速查](#八参数速查)
- [九、常见问题排查](#九常见问题排查)
- [十、迁移完成后的核对与交付](#十迁移完成后的核对与交付)
- [十一、安全注意事项](#十一安全注意事项)

---

## 一、交付物清单

你应该收到下面这些文件：

| 文件 | 说明 | 要不要动 |
| --- | --- | --- |
| `migrate_user_linux_amd64` | 用户表迁移程序 | **重命名**为 `migrate_user` |
| `migrate_org_linux_amd64` | 组织表迁移程序 | **重命名**为 `migrate_org` |
| `dmcheck_linux_amd64` | 达梦连接诊断工具 | **重命名**为 `dmcheck` |
| `.env` | 数据库连接配置 | **必须按现场环境修改** |
| `dmcheck.env` | 诊断工具自己的配置 | **必须按现场环境修改** |
| 本手册 | 就是这份 | — |
| `dmcheck-使用说明.md` | 诊断工具的单独说明 | — |

两个程序互不依赖：只迁一张表就只跑一个，另一张表出问题不影响它。

**不在交付物里、但现场必须具备的：**

- 一台 Linux 服务器（x86_64），有可写入的目录（推荐 `/opt/dm2mongo`）
- 能访问达梦端口和 MongoDB 端口的网络（不要指望能访问外网）
- 达梦账号（能查源表）+ MongoDB 账号（能写目标库）

> 需要的是 Linux 二进制，**不要在 Windows 上直接双击运行**（会提示无法识别的格式）。

---

## 二、开工前要凑齐的信息

改 `.env` 之前，先把下面这些问清楚。缺一项就一定会卡住，建议开工前一次性拿全。

| 需要的信息 | 举例 | 找谁要 |
| --- | --- | --- |
| 达梦 IP | `10.8.19.133` | 达梦 DBA |
| 达梦端口 | `5236` | 达梦 DBA |
| 达梦账号 / 密码 | 能 `SELECT` 源表的账号 | 达梦 DBA |
| 源表的模式名和表名 | 默认是 `DLPDATA.did_system_sync_user` / `_org` | 达梦 DBA |
| MongoDB IP | `10.8.27.151` | 应用运维 |
| MongoDB 端口 | `32017` | 应用运维 |
| MongoDB 账号 / 密码 / 认证库 | 账号一般建在 `admin` 库 | 应用运维 |
| MongoDB 目标库名 | 默认 `identity` | 应用运维 |
| 服务器 CPU 架构 | `uname -m` 看，见下一节 | 机房 / 你自己看 |
| 目标集合里有没有正式数据 | `drop` 模式会先删集合 | 应用运维 |

> **务必确认目标集合里没有别人正在用的数据。** 默认的 `-clean drop` 会先删掉目标集合再写入。

---

## 三、开工前环境检查

### 3.1 确认 CPU 架构（决定二进制能不能跑）

```bash
uname -m
```

| 输出 | 说明 |
| --- | --- |
| `x86_64` | ✅ 正常，交付的 amd64 版本可以直接用 |
| `aarch64` / `arm64` | ❌ 交付的 amd64 版本**跑不了**（会报 `cannot execute binary file`），需向提供方索取 arm64 版本 |

### 3.2 确认网络能到两个数据库

```bash
# 有 nc 的话用这个（推荐）
nc -vz 10.8.19.133 5236      # 达梦
nc -vz 10.8.27.151 32017     # MongoDB

# 没有 nc 就用 telnet
telnet 10.8.19.133 5236
telnet 10.8.27.151 32017
```

两个都要显示 `succeeded` / `Connected` 才能继续。

如果连不上，**先找网络和 DBA，不要动配置文件**——配置改得再对，端口不通也白搭。

### 3.3 确认宿主机权限

```bash
whoami                       # 当前账号
mkdir -p /opt/dm2mongo && echo "目标目录可写"
```

`/opt` 下建不了目录就换个位置（比如 `/home/你的账号/dm2mongo`），程序对安装路径没有要求。

---

## 四、部署

四条命令，复制粘贴即可：

```bash
# 1) 建目录（logs 必须建，程序不会自动创建）
mkdir -p /opt/dm2mongo/logs

# 2) 把三个文件上传到 /opt/dm2mongo

# 3) 加执行权限
chmod +x migrate_user migrate_org

# 4) .env 里有明文密码，收一下权限
chmod 600 .env
```

部署完的目录应该长这样：

```
/opt/dm2mongo/
├── migrate_user          # 可执行文件
├── migrate_org
├── .env                  # 连接配置（权限 600）
└── logs/                 # 日志目录
    ├── user.log
    └── org.log
```

上传方式随你：`scp`、SFTP 工具拖拽、跳板机 `rz` 都行。示例：

```bash
scp migrate_user migrate_org .env user@10.8.19.133:/opt/dm2mongo/
```

> ⚠️ **`logs/` 必须手动创建。** 程序只会创建日志文件本身，不会创建父目录，
> 目录不存在会直接报「打开日志文件失败」。

> 💡 程序会自动从**当前工作目录**向上逐级查找 `.env`。
> 所以只要 `.env` 和程序放在同一个目录，运行时就**不用带任何参数**。
> 但为了保险，本文档后面的命令都建议先 `cd /opt/dm2mongo`。

---

## 五、修改 .env（现场必做）

```bash
cd /opt/dm2mongo
vi .env
```

文件全长这样，一共 10 行有效配置：

```ini
# ---------------- 达梦数据库 ----------------
DM_HOST=10.8.19.133          # 改成现场达梦 IP
DM_PORT=5236                 # 达梦端口，一般是 5236
DM_USER=SYSDBA               # 改成现场达梦账号
DM_PASS=SYSDBA               # 改成对应密码

# ---------------- MongoDB ----------------
MONGO_HOST=10.8.27.151       # 改成现场 Mongo IP
MONGO_PORT=32017             # Mongo 端口，本项目是 32017（不是默认的 27017）
MONGO_DB=identity            # 目标库名
MONGO_USERNAME=root          # 改成现场 Mongo 账号
MONGO_PASSWORD=your_password # 改成对应密码
```

### 5.1 每一项要不要改

| 配置项 | 要不要改 | 填什么 |
| --- | --- | --- |
| `DM_HOST` | ✅ **必改** | 现场达梦 IP |
| `DM_PORT` | ⚠️ 核对 | 达梦端口，默认 `5236`（**不是 MySQL 的 3306**） |
| `DM_USER` | ✅ **必改** | 能查源表的达梦账号 |
| `DM_PASS` | ✅ **必改** | 对应密码 |
| `MONGO_HOST` | ✅ **必改** | 现场 MongoDB IP |
| `MONGO_PORT` | ⚠️ 核对 | 本项目是 `32017`，不是 MongoDB 默认的 27017 |
| `MONGO_DB` | ⚠️ 核对 | 目标库名，默认 `identity` |
| `MONGO_USERNAME` | ✅ **必改** | MongoDB 账号；**没有账号就整行删掉**（表示免认证） |
| `MONGO_PASSWORD` | ✅ **必改** | MongoDB 密码 |
| `MONGO_AUTH_SOURCE` | 一般不用 | 账号所在的库，默认 `admin`。认证失败时才需要改 |
| `DM_SCHEMA` | 一般不用 | 登录后的默认模式。**留空即可**，源表名自带模式前缀 |

`MONGO_USERNAME` / `MONGO_PASSWORD` 如果都不填，程序按免认证方式连接。反过来，MongoDB 要认证但你没填账号，一定会报 `Authentication failed`。

### 5.2 格式规则（照着填，别创新）

这是**配置文件**，不是 shell 脚本：

1. 一行一项，写成 `键=值`。**不要加引号，不要写 `export`。**
2. `=` 两边多打了空格没关系，程序会自动去掉。
3. `#` 或 `;` **开头的整行**是注释（上面模板里的中文说明就是注释，留着或删掉都行）。
4. **行尾注释必须在 `#` 前面留一个空格**：`DM_PORT=5236   # 达梦端口` ✅
   ⚠️ 密码里如果含 `#`，`#` 前面**千万不要有空格**，否则会被当成注释截断，密码就错了。
5. **值不要用引号包**（程序会把两端引号脱掉）。只有一种情况需要引号：密码本身以空格开头或结尾。
6. 键名必须**全大写**，且不要写错：`MONGO_USERNAME` 不等于 `MONGO_USER`（后者也认，但别混用）。
7. 文件必须保存为 `.env`（注意前面那个点，别存成 `env` 或 `.env.txt`）。
8. 在 Windows 上编辑过也没关系，开头 BOM 字符会被自动忽略。

**填完记得保存，然后立刻自检（下一步），不要直接开跑全量。**

### 5.3 改完立刻自检

```bash
cd /opt/dm2mongo
./migrate_user -count-only
```

**正常输出长这样：**

```
  用户表迁移  "DLPDATA"."did_system_sync_user" → identity.user
  配置文件    /opt/dm2mongo/.env
  达梦        dm://SYSDBA:***@10.8.19.133:5236
  MongoDB     mongodb://root:***@10.8.27.151:32017/?authSource=admin (db=identity)

  源表 "DLPDATA"."did_system_sync_user" 共 398,726 条记录
```

逐行核对三件事：

1. **「配置文件」这一行必须是你刚改的那份**（路径不对说明放错位置了）。
2. 达梦和 MongoDB 两行的密码都显示 `***`，这是正常的，**程序不会打印明文密码**。
3. 最后一行是源表真实条数，跟 DBA 给你的数字对得上就没问题。

`-count-only` 会执行这几件事：读配置 → 连达梦 → 数条数 → 退出。**它不会写入任何数据。**

> ⚠️ 注意：屏幕上虽然显示了 MongoDB 那一行，但 `-count-only` **并没有真正连接 MongoDB**
> （那行只是回显配置）。想验证 MongoDB，看第六节的第 ②③ 步。
>
> **如果这里就报错了，直接翻第九节排错表，不要继续往下走。**

---

## 六、执行顺序（四步走）

**不要一上来就全量跑。** 按这个顺序走，出错最容易定位：

```bash
# 终端一执行，迁移结束再关闭
kubectl -n topsec-topaiop port-forward svc/redis 7379:7379
kubectl -n topsec-topaiop port-forward svc/mongodb 27017:27017

# 以下都在终端二执行
cd /opt/dm2mongo

# ⓿ 先跑诊断工具，确认「配置 + 网络 + 达梦账号 + 库内对象」都没问题
./dmcheck

# ① 数条数：验证「配置 + 网络 + 达梦账号 + 表名」       —— 只碰达梦，不写数据
./migrate_user -count-only
./migrate_org  -count-only

# ② 试跑 200 条，不写库：验证「字段映射 + Mongo 连通与认证」 —— 什么都不写
./migrate_user -dry-run -limit 200

# ③ 试跑 200 条，真写库：验证「Mongo 写入权限」           —— 会写 200 条
./migrate_user -limit 200 -clean none

# ④ 去 Mongo 里核对这 200 条没问题后，正式全量
./migrate_user -log logs/user.log
./migrate_org  -log logs/org.log
```

每一步分别验证什么：

| 步骤 | 验证内容 | 不验证什么 |
| --- | --- | --- |
| ⓿ `./dmcheck` | 配置解析、网络、达梦账号密码、模式与源表是否存在 | **完全没碰 MongoDB** |
| ① `-count-only` | 配置文件、网络、达梦账号、源表名 | **完全没碰 MongoDB** |
| ② `-dry-run` | 读达梦数据、字段转换、Mongo 连通与密码认证 | 写入权限（一条都不写） |
| ③ `-limit 200 -clean none` | Mongo 建集合 / 写入权限 | 全量数据量下的稳定性 |
| ④ 全量 | 全部 | — |

第 ③ 步跑完，去 MongoDB 里查这 200 条，**重点看 `user_status` / `sort_no` 是不是字符串**（比如 `"1"` 而不是 `1`），这一点搞错应用侧会读不出数据。核对方法见第十节。

### 正式跑的几个建议

- **`-clean` 用默认的 `drop` 就行**：先删集合再写入，每次跑都是干净完整的副本，
  程序会自动快照并重建原有索引。只有在「目标集合的索引是别人手工建的、不想被删」时才用 `-clean delete`。
- **两张表可以先后跑，也可以开两个终端同时跑**（源表不同，互不干扰）。
  但如果 Mongo 写入是瓶颈，跑完一张再跑下一张更稳。
- **40 万条预期 1～3 分钟。** 超过 10 分钟还没跑完基本就是卡住了，看日志。
- **重跑很安全**：`drop` 模式每次都会先清空，断了、报错了直接重跑即可，不会产生重复数据。

### 如果源表名跟默认的不一样

默认源表是 `DLPDATA.did_system_sync_user` / `DLPDATA.did_system_sync_org`。
**如果现场的模式名或表名不同**，不用改程序，运行时带上 `-dm-table` 即可：

```bash
# 假设现场是 NEWSCHEMA.did_system_sync_user
./migrate_user -count-only -dm-table NEWSCHEMA.did_system_sync_user
```

确认没问题后，后面每一步都要带同一个 `-dm-table` 参数。

> 区分「是表名问题还是网络/账号问题」的万能方法——换一张达梦自带的系统表当靶子：
> ```bash
> ./migrate_user -count-only -dm-table SYS.SYSOBJECTS
> ```
> 能数出条数，就证明**配置、网络、账号这条链路完全正常**，问题只出在表名或权限上。

---

## 七、进度、日志与中断

### 7.1 进度条

在终端里直接运行，会原地刷新四行进度块：

```
  迁移中  "DLPDATA"."did_system_sync_user" → identity.user
  读取   [██████████████░░░░░░░░░░░░░░░░░░░░]   42.13%   168,520 / 400,000 条
  写入   [██████████████░░░░░░░░░░░░░░░░░░░░]   42.13%   168,200 条    失败 0 条
  速度   读 12,430 条/秒 · 写 11,980 条/秒      已用 00:00:17   剩余 00:00:18
```

看到 `失败 0 条` 且百分比走到 100% 就是正常完成。

**重定向到文件、`nohup`、后台运行时会自动降级**为按百分比打点，不会刷出满屏控制字符：

```
  [进行中]  45%  读取 180,000/400,000 条  写入 179,800 条  失败 0 条  用时 00:00:18  均速 10,000 条/秒
```

### 7.2 后台长跑

数据量小的话直接前台跑就行。要挂后台：

```bash
cd /opt/dm2mongo
nohup ./migrate_user -log logs/user.log </dev/null >logs/user.progress.log 2>&1 &
tail -f logs/user.progress.log     # 另开一个终端看进度
tail -f logs/user.log              # 看明细日志
```

想让进度条保持原地刷新的效果，用 `tmux`：

```bash
tmux new -s mig
cd /opt/dm2mongo && ./migrate_user -log logs/user.log
# Ctrl+B 再按 D 脱离；tmux attach -t mig 回来
```

### 7.3 中断了怎么办

`Ctrl+C`（或 `kill`）会触发优雅退出：在**批次边界**停止，打印中断原因后以非 0 退出码结束。
**不会留下写了一半的文档**——每条记录都是完整写入的。

因为默认 `-clean drop` 会先清空集合，中断后目标集合是空的或只有部分数据，
**直接重跑即可**，不需要做任何额外清理。

### 7.4 读日志

日志文件是纯文本，直接看：

```bash
tail -50 logs/user.log           # 看最后 50 行
grep -i "error\|失败" logs/user.log   # 找错误
```

想在终端上同时看到明细日志，加 `-verbose`。

---

## 八、参数速查

### 8.1 现场常用（记住这 5 个就够）

| 参数 | 用途 |
| --- | --- |
| `-count-only` | 只数条数，不迁移。**排错第一步** |
| `-dry-run` | 只读不写，验证字段映射 |
| `-limit 200` | 只迁移前 200 条，用来试跑 |
| `-log logs/xxx.log` | 把明细写进日志文件 |
| `-dm-table 模式.表名` | 源表名跟默认不一样时指定 |

查看完整帮助：

```bash
./migrate_user -h
```

### 8.2 其余参数（一般不用动）

两个程序的参数**完全一致**，只有源表默认值不同。

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-env <path>` | 空 | 配置文件路径。留空则自动从当前目录逐级向上查找 `.env` |
| `-dm-table <name>` | 见上表 | 达梦源表全限定名，**不带引号** |
| `-dm-case <mode>` | `as-is` | 标识符大小写折叠：`as-is` / `upper` / `lower`。表名报「不存在」时试着换 |
| `-dm-schema <name>` | 空 | 达梦模式名。留空取 `.env` 的 `DM_SCHEMA`，都空则不指定 |
| `-mongo-db <name>` | 空 | 目标库名。留空取 `.env` 的 `MONGO_DB`，默认 `identity` |
| `-mongo-collection <name>` | `user` / `organization` | 目标集合名 |
| `-clean <mode>` | `drop` | 迁移前处理目标集合：`drop` 删集合 / `delete` 清数据保索引 / `none` 不清 |
| `-batch <n>` | `1000` | 每批写入条数 |
| `-page-size <n>` | `20000` | 达梦分页读取页大小。设 `0` 改为单次流式读取 |
| `-workers <n>` | `2` | 并发写入协程数。写入慢时可调到 `4` |
| `-row-prefetch <n>` | `500` | 达梦驱动每批预取行数，影响读取内存 |
| `-ensure-index` | `true` | 迁移完成后补建常用索引 |
| `-no-progress` | `false` | 关闭进度条，改为纯文本输出 |
| `-verbose` | `false` | 终端打印明细日志 |
| `-timeout <dur>` | `60m` | 整次迁移超时时间，到点自动中止 |
| `-conn-timeout <dur>` | `15s` | 建连超时 |

---

## 九、常见问题排查

**遇到任何连不上的问题，第一件事是跑 `./dmcheck`。**
它会把配置、网络、账号密码、模式与源表逐项验证，并直接给出原因和处理办法，
比对着下表猜快得多。详细说明见 [dmcheck-使用说明.md](dmcheck-使用说明.md)。

只有它给出的结论还不够用时，再查下表：

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `未找到含数据库配置的 .env` | `.env` 不在当前目录，或文件里没有 `DM_HOST` / `MONGO_HOST` | `cd /opt/dm2mongo` 再跑；或用 `-env /opt/dm2mongo/.env` 显式指定 |
| `缺少达梦连接地址 DM_HOST` | `.env` 里少了 `DM_HOST` 这一行 | 补上。注意程序**不支持** `DM_JDBC_URL` / `mongodb://...` 这类连接串写法 |
| `缺少达梦用户名 DM_USER` | 没填账号 | 填 `DM_USER` |
| `打开配置文件失败` | 路径写错 / `.env` 不存在 | 确认文件名就是 `.env`；`ls -la /opt/dm2mongo/.env` |
| `Error 6001 网络通信异常` | 到不了达梦端口 | `nc -vz <DM_HOST> 5236` 确认；通常是没连内网/VPN、防火墙没放通、或达梦服务没起 |
| `用户名或密码错误` | 账号密码不对，或密码里有特殊字符被转义 | 先跑 `./dmcheck`。它会判断是「密码真的错」还是「密码被转义」；后者需要联系提供方换一个修复版程序 |
| `Connection refused` | 端口不对或服务没起 | 核对 `DM_PORT`（`5236`）/ `MONGO_PORT`（本项目 `32017`） |
| `无效的模式名[XXX]` | 这个实例里没有 `XXX` 模式 | 说明源表名跟默认的不一样。用 `-dm-table 实际模式.实际表名`；先用 `-dm-table SYS.SYSOBJECTS -count-only` 排除网络/账号因素 |
| `无效的表或视图名` | 表不存在，或大小写折叠对不上 | 找 DBA 确认表名；再试 `-dm-case upper` / `-dm-case lower` |
| `用户或密码错误` / 达梦认证失败 | 账号密码不对 | 核对 `DM_USER` / `DM_PASS`；注意密码里若含 `#`，前面不能有空格（会被当注释截断） |
| `Authentication failed` | Mongo 账号不在认证库里 | 把 `MONGO_AUTH_SOURCE` 改成账号实际所在的库（常见是 `admin`） |
| 连接 Mongo 超时 | 端口不通 | `nc -vz <MONGO_HOST> <MONGO_PORT>` 确认；注意本项目是 `32017` 不是 `27017` |
| `打开日志文件失败` | `logs/` 目录不存在 | `mkdir -p /opt/dm2mongo/logs` |
| `Permission denied` | 没有执行权限 | `chmod +x migrate_user migrate_org` |
| `cannot execute binary file` | 服务器架构不是 x86_64 | `uname -m` 确认；是 `aarch64` 就要索取 arm64 版本 |
| 写入很慢（< 500 条/秒） | 批量太小 / 网络延迟高 | 加 `-batch 2000 -workers 4`；先确认不是网络瓶颈 |
| 内存占用高 | 分页或预取过大 | 加 `-page-size 5000 -row-prefetch 200` |
| 应用侧读 Mongo 报类型错误 | `user_status` / `sort_no` 写成了数字 | 见第十节，这两个字段必须是字符串 |
| 跑一半中断 | 超时 / 手动 Ctrl+C | 直接重跑；数据量大时加 `-timeout 2h` |

**排错通用套路：先 `./migrate_user -count-only`。**
它报错就按上表处理；它通过但全量失败，就把问题锁定在 MongoDB 侧（看第 ②③ 步）。

---

## 十、迁移完成后的核对与交付

### 10.1 核对条数（源 vs 目标）

**源表条数**：`-count-only` 已经打印了。

**目标集合条数**：在能连 MongoDB 的机器上执行（`mongosh` 是 MongoDB 官方客户端）：

```bash
mongosh "mongodb://账号:密码@10.8.27.151:32017/?authSource=admin"
```

进去之后：

```javascript
use identity
db.user.countDocuments()            // 应该等于源表条数
db.organization.countDocuments()
db.user.findOne()                   // 抽一条看字段
```

抽出来那条记录，重点确认三件事：

1. `user_status` / `sort_no` 是**字符串**（`"1"`），不是数字（`1`）
2. `linke_man_email` 字段名是**少一个 n** 的（这是目标结构定的，别改）
3. `serial_num` 是空字符串 `""`（源表没有这个字段，等业务系统补）

### 10.2 跑完的汇总输出

正式跑完会打印这样一个统计块（下面是成功的样子）：

```
──────────────────────────────────────────────────────────────────
  迁移完成 · 用户表
──────────────────────────────────────────────────────────────────
  源表        "DLPDATA"."did_system_sync_user"   398,726 条
  目标集合    identity.user                      398,726 条
  清空旧数据  398,726 条
  读取        398,726 条
  写入        398,726 条
  耗时        00:01:42   平均 3,909 条/秒
──────────────────────────────────────────────────────────────────
```

**判断成功的标准（两个都要满足）：**

1. **没有「失败」这一行**。⚠️ 注意：程序**只在真有失败时才打印「失败 N 条」**，
   所以「看不到失败那一行」才是正常的，不是漏打印了。
2. **「源表」和「目标集合」条数一致**，且等于 `-count-only` 数出来的数。

如果出现下面这些行，说明有问题：

| 输出 | 含义 | 处理 |
| --- | --- | --- |
| `失败        3 条` | 有 3 条读/写失败 | 查日志里的具体错误原因 |
| `提示  写入 X 条与目标集合实际 Y 条不一致` | 目标集合里有多余数据（通常是之前跑过没清空） | 用默认的 `-clean drop` 再跑一次 |

`-dry-run` 的输出会不一样，标题是「试运行结束（未写入数据）」，且**没有「目标集合」和「清空旧数据」两行**——这是正常的，因为试运行本来就不写库。

### 10.3 需要回传给提供方的信息

万一需要排查，把下面这些一次性发回：

1. `./migrate_user -count-only` 和 `./migrate_org -count-only` 的**完整输出**
2. 正式跑完的**完整统计块**（从 `迁移完成` 那一行到分隔线结束，整块截图）
3. 报错时的**完整错误信息**（不要只发一句「跑不起来」）
4. 目标集合的 `countDocuments()` 结果
5. 日志文件：`logs/user.log`、`logs/org.log`
6. 服务器信息：`uname -a`、`cat /etc/os-release`

**回传前记得把日志里的密码相关内容去掉**（正常情况下程序不会打印明文密码，但仍建议过一眼）。

### 10.4 需要重跑 / 补跑

直接再跑一次即可。默认 `drop` 模式会先清空目标集合，不会产生重复数据。

只想重跑一张表，就跑对应的那一个程序，另一个不用动。

---

## 十一、安全注意事项

1. **`.env` 里有明文密码**，权限设成 `600`（`chmod 600 .env`），不要通过微信、邮件等渠道外发，
   用完建议删除或改密码。
2. **不要用 root 跑**（除非现场规定必须如此）。用普通账号 + 可写的部署目录即可。
3. **确认目标集合里没有正在使用的数据**：默认 `-clean drop` 会先删掉整个集合。
   不确定就用 `-clean delete`（只清数据、保留索引）或 `-clean none`（不动已有数据）。
4. **迁移窗口建议避开业务高峰**，虽然程序对源表只做读取，但会占用达梦连接和网络带宽。
5. **不要中途拔网线 / 强杀进程**。`Ctrl+C` 是安全的（批次边界退出），`kill -9` 可能让日志不完整。

---

## 附：一次性速查卡

```bash
# ---- 部署 ----
mkdir -p /opt/dm2mongo/logs && cd /opt/dm2mongo
mv migrate_user_linux_amd64 migrate_user && mv migrate_org_linux_amd64 migrate_org
chmod +x migrate_user migrate_org && chmod 600 .env

# ---- 改配置 ----
vi .env      # 至少改 DM_HOST / DM_USER / DM_PASS / MONGO_HOST / MONGO_USERNAME / MONGO_PASSWORD

# ---- 四步走 ----
./migrate_user -count-only                        # ① 数条数
./migrate_user -dry-run -limit 200                # ② 试读，不写
./migrate_user -limit 200 -clean none             # ③ 试写 200 条，去 Mongo 核对
./migrate_user -log logs/user.log                 # ④ 全量
./migrate_org  -log logs/org.log

# ---- 排错 ----
./migrate_user -count-only                        # 报错先看这里
./migrate_user -count-only -dm-table SYS.SYSOBJECTS   # 验证「配置+网络+账号」是否通
./migrate_user -h                                 # 查看全部参数
```

> 编译方法、代码结构、字段映射等开发内容见 [DEVELOP.md](DEVELOP.md)。
