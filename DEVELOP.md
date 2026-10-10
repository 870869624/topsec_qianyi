# 达梦 → MongoDB 数据迁移工具 · 开发与维护文档

> **这份文档是给开发者的。** 现场实施人员请阅读 [README.md](README.md)。
>
> 现场只会收到编译好的二进制和 `.env`，本文涉及的交叉编译、代码结构、字段映射
> 都不在现场交付范围内。

---

## 目录

- [一、代码结构](#一代码结构)
- [二、交叉编译](#二交叉编译)
- [三、配置解析实现](#三配置解析实现)
- [四、字段映射要点](#四字段映射要点)
- [五、新增一张表的迁移程序](#五新增一张表的迁移程序)
- [六、测试数据工具](#六测试数据工具)
- [七、达梦连接诊断工具 dmcheck](#七达梦连接诊断工具-dmcheck)
- [八、发布交付前的检查清单](#八发布交付前的检查清单)

---

## 一、代码结构

```
demo/
├── build.sh                 交叉编译脚本
├── migrate_user/main.go     独立可执行：did_system_sync_user → identity.user
├── migrate_org/main.go      独立可执行：did_system_sync_org  → identity.organization
├── dmcheck/main.go          独立可执行：达梦连接诊断工具（单文件，不依赖 syncbase）
├── dmcheck/dmcheck.env      诊断工具自带的配置模板
└── syncbase/                两者共用的内部包，两个 main 之间零依赖
    ├── env.go       .env 定位与解析、`LoadConfig` 统一入口、达梦 / Mongo 配置结构体
    ├── conn.go      达梦 + Mongo 连接建立
    ├── progress.go  实时进度条（TTY 直绘 + 非 TTY 降级）
    ├── runner.go    统计 → 清空 → 分页读取 → 并发批量写入 → 校验
    ├── indexes.go   索引快照 / 重建
    ├── idents.go    达梦标识符大小写处理
    ├── value.go     空值与类型转换工具（`Str` / `NumStr` / `NullTime`）
    └── logger.go    日志（写入时自动避让进度条，不冲花界面）
```

> **运行上完全解耦，代码上共用底座。** 两张表共享约 400 行基础设施（配置、连接、
> 进度条、批量写入），复制两份维护成本不划算；但字段映射、数据量、重跑节奏各不相同，
> 所以做成两个可独立执行的 `main`。

配置只需要一次 `syncbase.LoadConfig` 调用，返回 `*Config`（含 `DM`、`Mongo`、`File`），
两个 main 里各占约 10 行：

```go
cfg, err := syncbase.LoadConfig(*envPath)   // 留空则自动查找 .env
if err != nil {
    return err
}
if *schemaName != "" {                      // 命令行参数优先级高于配置文件
    cfg.DM.Schema = *schemaName
}
if *mongoDB != "" {
    cfg.Mongo.Database = *mongoDB
}
```

---

## 二、交叉编译

### 2.1 前置条件

- **Go 1.22 或更高版本**（模块 `go.mod` 声明 `go 1.22`）。
- **不需要** Linux 环境、Docker、交叉编译工具链。
- 达梦驱动 `godoes/gorm-dameng` 和 `mongo-driver` **都是纯 Go 实现**（无 `import "C"`），
  所以关掉 CGO 就能静态链接，不会出现 `GLIBC_2.28 not found` 这类问题。

### 2.2 一键脚本（推荐）

在**模块根目录**（`go.mod` 所在目录）执行：

```bash
bash demo/build.sh                           # 默认只出 linux/amd64
bash demo/build.sh linux/amd64 linux/arm64   # 一次出两个架构
OUT_DIR=/tmp/out bash demo/build.sh linux/amd64   # 自定义输出目录
```

产物落在 `demo/dist/`：

```
demo/dist/migrate_user_linux_amd64
demo/dist/migrate_org_linux_amd64
demo/dist/dmcheck_linux_amd64
```

脚本做的事很简单：设置 `CGO_ENABLED=0` + `GOOS` + `GOARCH`，用 `-trimpath -ldflags "-s -w"` 编译
（去掉符号表和调试信息，体积从 17MB 降到 12MB；`dmcheck` 不引 mongo-driver，只有 7.7MB）。
要新增迁移程序，改脚本里的 `APPS=(...)` 一行即可。

### 2.3 手动命令

```bash
# Linux x86_64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w" -o migrate_user ./demo/migrate_user/

# Linux ARM64（麒麟 / 统信等国产 ARM 服务器）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags "-s -w" -o migrate_user_arm64 ./demo/migrate_user/
```

Windows 的 CMD / PowerShell 里写法：

```powershell
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -trimpath -ldflags "-s -w" -o migrate_user.exe ./demo/migrate_user/
```

> 注意 `go build` 的包路径是 `./demo/migrate_user/`，**必须在模块根目录执行**。
> 如果在 `demo/` 目录里直接 `go build ./migrate_user`，会因为找不到 `go.mod` 而报错。

### 2.4 架构怎么选

在目标 Linux 服务器上执行 `uname -m`：

| `uname -m` 输出 | 对应 `GOARCH` | 典型场景 |
| --- | --- | --- |
| `x86_64` / `amd64` | `amd64` | 绝大多数 Intel / AMD 服务器、虚拟机、容器 |
| `aarch64` / `arm64` | `arm64` | 鲲鹏、飞腾、AWS Graviton |
| `loongarch64` | `loong64` | 龙芯（Go 1.19+） |
| `mips64le` | `mips64le` | 部分国产化环境 |

### 2.5 验证产物

```bash
# 应输出 ELF 64-bit LSB executable, x86-64
file migrate_user_linux_amd64

# 没有 file 命令时用魔数判断：7f 45 4c 46 就是 ELF
head -c 4 migrate_user_linux_amd64 | od -An -tx1
```

---

## 三、配置解析实现

`syncbase/env.go` 里的 `loadEnv` 行为（现场文档里的「格式规则」就是从这里来的）：

- **查找顺序**：当前工作目录下的 `demo/` 子目录及其各级父目录 → 当前工作目录及其各级父目录。
  只接受「含 `DM_HOST` 或 `MONGO_HOST`」的那份文件——项目根目录的 `.env` 存的是 SM2/SM4 密钥，
  会被自动跳过。
- **解析规则**：`key=value`；键值两端 `TrimSpace`；值两端引号被脱掉；
  BOM 被忽略；`#` / `;` 开头的整行为注释；
  行尾注释只在 `" #"`、`"\t#"`、`" ;"`（前面有空白）时生效，被引号包裹的值视为字面量原样保留。
- **不支持 `export`**（这是配置文件，不是 shell 脚本）。写 `export DM_HOST=1.2.3.4`
  会导致键名变成 `export DM_HOST`，等同于没配。
- **环境变量覆盖**：凡 `DM_` / `MONGO_` 前缀的系统环境变量都会覆盖同名项。
  好处是不用维护白名单，新增配置项自动生效；代价是运行环境里如果存在同名变量会被意外覆盖。

> 曾经存在 `parseDMURL`（手写解析 `jdbc:dm://...`）、`injectMongoCredential`
> （URL 往返解析注入账号）、`LoadEnv` + `LoadEnvAuto` + `findUp` 三层查找、
> 以及 `envKeys` / `dbConfigKeys` 两个白名单——这些都已删除。
> 当时 `injectMongoCredential` 里藏着一个 bug：连接串自带账号时会提前 `return`，
> 跳过 `authSource` 补充逻辑。现在改为 `SetHosts` + `options.Credential` 结构化连接，
> 密码里含 `@` `:` `/` 也不会拼坏。
>
> `SafeDSN` / `SafeURI` **必须始终脱敏**（输出 `***`），这是安全边界，不要为了调试改掉。

### 3.1 DSN 必须字面量拼接，不能用 url.URL（踩过的坑）

`DMConfig.DSN()` 一度用 `url.URL{User: url.UserPassword(...)}` 构造，看着很规范，实则致命：

> **达梦驱动的 `parseDSN`（`gorm-dameng/dm8/n.go`）只做字符串切分，不做 URL 解码。**
> 它按「最后一个 `@` 切账号、第一个 `:` 切密码」取值，`c.password` 拿到的就是字面量。

而 `url.URL.String()` 会把密码里的 `@` `?` `:` `#` `%` `空格` 等字符转义成 `%XX`。
两者相遇，服务端收到的密码就成了 `P%40ss` 这种形态，
**现象是「账号密码明明正确，却一直报用户名或密码错误」**。

字面量拼接是安全的：驱动切分 query 用的是最后一个 `?`，而密码即使含 `?` 也一定排在它前面，
所以只要连接串带着查询参数（`DSN()` 里恒有 `appName`），密码中出现任何字符都不会被切错。

实测（在测试实例上建了一个密码为 `P@ss#2024` 的账号）：

| 构造方式 | 驱动实际收到的密码 | 结果 |
| --- | --- | --- |
| `url.URL` + `UserPassword` | `P%40ss%232024`（13 字符） | ✗ `Error -2501: 用户名或密码错误` |
| 字面量拼接（当前实现） | `P@ss#2024`（9 字符） | ✓ 认证成功 |

密码里**不含**需要转义的字符时，两种构造方式产出的 DSN 完全相同，所以这个修复对现有环境零影响。

> 同类问题的排查工具见第七节 `demo/dmcheck`。

---

## 四、字段映射要点

这几处是踩过的坑，改代码时务必留意：

1. **`user_status`（用户表）和 `sort_no`（组织表）必须转成字符串。**
   达梦里它们是 `INT`，但 `pkg/model/identity` 下 `User.UserStatus` / `Organization.SortNo`
   声明的是 `string`。直接写数字会让应用侧反序列化报类型错误。
   代码里用 `syncbase.NumStr()` 做转换。

2. **`linkeman_email` → `linke_man_email`（少一个 `n`）。**
   目标集合的字段名拼写和源表不一致，这是目标 model 定的，别「顺手纠正」。

3. **达梦标识符大小写要严格照搬。**
   `did_system_sync_user` 的建表 DDL 里，表名和大多数字段是小写加双引号，
   但 `"JOB"`、`"PASSWORD"` 是大写加双引号。`userColumns` 里已经按实际情况写好，
   不要为了「看起来整齐」统一大小写。

4. **没有对应列的字段怎么填：**
   - `serial_num`（流水号）：源表没有，写空字符串，后续由业务系统补。
   - `created_at` / `updated_at`：写入时的 Unix 秒时间戳。
   - `_id`：每条生成新的 `ObjectID`。

5. **空值处理。** 达梦的 `NULL` 通过 `sql.NullString` / `sql.NullInt64` 读取，
   字符串统一写空串；时间字段用 `syncbase.NullTime`，`NULL` 时写 `null` 而不是零值时间。

6. **改字段顺序时必须同步改两处**：`xxxColumns` 数组和 `xxxRow.ScanTargets()`，
   两者顺序**严格一一对应**，错位会导致数据串列且不报错（最危险的一类 bug）。
   程序启动时会做一次列数自检，但只校验数量、不校验顺序。

---

## 五、新增一张表的迁移程序

1. 在 `demo/` 下建一个新目录，照着 `migrate_user/main.go` 写三部分：
   `Columns`（达梦列名数组）、`ScanTargets()`（扫描目标指针）、`Document()`（目标 bson 结构）。
2. `Document()` 对应的 struct 的 bson tag 要与 `pkg/model/identity` 里的 model 一一对应。
3. 在 `build.sh` 的 `APPS=(...)` 里加一行。
4. 编译后在真实环境按 README 的四步走验证一遍。

---

## 六、测试数据工具

`demo/_seed` 和 `demo/_verify` 是**联调测试用的辅助程序，不属于交付物**。
目录名以下划线开头，Go 工具会跳过它们，所以不会影响 `go build ./demo/...` 和 `demo/build.sh`。

### 6.1 `demo/_seed`：在达梦里造源表和测试数据

源库没有 `DLPDATA` 模式（或者表是空的）时，用它在达梦里建出可迁移的数据。

```bash
go run ./demo/_seed                          # 默认：用户表 500 条 + 组织表 300 条
go run ./demo/_seed -rows 2000 -org-rows 800 # 自定义条数
go run ./demo/_seed -rows 400000 -org-rows 100000   # 按真实量级压测
go run ./demo/_seed -clean=false             # 追加数据而不清空
go run ./demo/_seed -drop                    # 只删掉这两张表后退出
go run ./demo/_seed -schema OTHER_SCHEMA     # 换模式名
```

行为要点：

- 建表 DDL **照搬** `达梦表结构/*.txt`，列名大小写、列顺序、类型完全一致；
  唯一改动是去掉 `STORAGE(ON "DLP_DATA")`（原库的表空间，目标实例不一定有）。
- 模式或表已存在则跳过创建，不会报错。
- `-clean`（默认开）在 `DELETE` 前会先把原有条数打出来，避免误删时毫无察觉。
- 写数据用一个事务 + 预编译语句，500+300 条约 1 秒。

生成的数据**刻意覆盖边界情况**（这是它相对「随便插几条」的价值所在）：

| 覆盖点 | 具体构造 |
| --- | --- |
| NULL 列 | `user_status` 每 13 条一条 NULL；`email` 每 5 条一条；三个时间字段各自独立的 NULL 分布 |
| INT → 字符串 | `user_status` / `sort_no` 是 INT，含 NULL，用来验证转换 |
| 中文字符 | 姓名、岗位、部门名、地址全用中文 |
| 引号与转义 | `他说："这个字段要小心"；她答：'我知道'`、含反斜杠与制表符 |
| 超长文本 | `comments` 480 字（验证 VARCHAR(2000) 与 Mongo 长串） |
| 特殊密码 | `P@ss#2024&word'quote\backslash`、`Pass#2024`、bcrypt 风格串 |
| 大写标识符 | 写入 `"JOB"` / `"PASSWORD"`，验证大写列名不被折叠 |
| 字段映射 | `linkeman_email` 用可辨识值 `linkeman00001@example.com`，核对目标 `linke_man_email` |

### 6.2 `demo/_verify`：抽查迁移结果

```bash
go run ./demo/_verify
```

打印几条样本文档，并对易错字段做断言式自检：

- `user_status` / `sort_no` 是字符串，且库里不存在数字型的
- 目标集合里有 `linke_man_email`、没有拼错的 `linkeman_email`
- `serial_num` 是空字符串
- NULL 时间写成了 `null` 而不是零值时间
- 中文、引号、长文本完整保留

### 6.3 一次完整的联调流程

```bash
go run ./demo/_seed                                  # ① 造数据
go run ./demo/migrate_user -count-only               # ② 验配置/网络/表名
go run ./demo/migrate_user -dry-run -limit 20        # ③ 验字段映射，不写库
go run ./demo/migrate_user                           # ④ 真迁移
go run ./demo/migrate_org
go run ./demo/_verify                                # ⑤ 抽查结果
```

> ⚠️ 注意：迁移默认 `-clean drop`，会先删掉目标集合再写入。
> 在别人可能在用的环境上跑之前，先确认目标集合是空的或可丢弃的。

---

## 七、达梦连接诊断工具 dmcheck

现场报「连不上达梦」时用它定位。单文件独立程序，**不依赖 `syncbase`**，
自带一份极简解析（复刻 `env.go` 的规则），所以即使主程序被改坏它也照样能跑。

```bash
go run ./demo/dmcheck                  # 默认找 ./dmcheck.env，其次 ./.env 并逐级向上
go run ./demo/dmcheck -config-only     # 只查配置，不发起任何网络连接
go run ./demo/dmcheck -pass '密码'     # 绕开配置文件，直接验证密码本身对不对
go run ./demo/dmcheck -show-secret     # 报告里显示账号密码明文（现场核对输错时用）
go run ./demo/dmcheck -out -           # 只在屏幕输出，不写报告文件
```

把连接拆成 6 步逐级判定，并把错误翻译成人话：

| 步骤 | 检查内容 | 能发现的典型问题 |
| --- | --- | --- |
| 1 | 配置文件定位 | 改对了 A 文件，程序读的却是 B 文件 |
| 2 | 解析与字符体检 | 首尾空白、不可见字符（U+00A0 / U+200B）、全角字符、行尾注释把密码截断 |
| 3 | DSN 构造对比 | 密码含 `@ ? : # %` 空格被 URL 转义（即 3.1 那个坑） |
| 4 | DNS + TCP | 不在内网、端口没开、主机名解析不了 |
| 5 | 驱动 + 认证 | 驱动未注册、密码错、账号被锁、密码过期、会话数满 |
| 6 | 库内连通性 | 模式不存在、源表不存在、账号无查询权限 |

两个设计细节值得留意，改这个工具时别丢：

- **不瞎试密码。** 只有确认密码含会被转义的字符时，才额外做第二次登录尝试（用于 A/B 对照），
  避免反复失败把账号锁掉。
- **报告必须真脱敏。** 密码统一显示为 `******（N 个字符）`。注意不能用
  `strings.Replace(dsn, password, "***", 1)`：用户名和密码恰好相同时
  （`SYSDBA`/`SYSDBA`）会替换错位置，把明文密码漏进报告；现在是重建一个脱敏 DSN。

另外，**结论区的措辞比中间过程重要**——现场只看得懂结论。每条 `finding` 都要带上
「是什么问题 + 怎么办」，只报错误不给建议等于没诊断。

---

## 八、发布交付前的检查清单

给现场发版本前，逐条确认：

- [ ] `gofmt -l ./demo/` 无输出
- [ ] `go vet ./demo/...` 干净
- [ ] `bash demo/build.sh linux/amd64` 构建成功，`file` 确认是 ELF
- [ ] **代码改动后重新编译了 `dist/` 里的二进制**（很容易忘，旧二进制不会包含新逻辑）
- [ ] `dist/` 里的三个程序都同步给了现场：`migrate_user`、`migrate_org`、`dmcheck`
- [ ] `dmcheck.env` 与当前 `.env` 的字段名保持一致（两边都要给现场）
- [ ] `.env` 里的地址是现场地址还是需要现场改的模板？**跟现场沟通清楚**
- [ ] `.env` 权限提示写进了交付说明（`chmod 600`）
- [ ] `demo/.env` 已被 git 跟踪且含明文密码 → 建议移出版本库，只提交 `.env.example`
- [ ] README.md 里的默认源表名与实际交付版本一致
- [ ] 确认目标服务器 CPU 架构，必要时一并提供 arm64 版本
