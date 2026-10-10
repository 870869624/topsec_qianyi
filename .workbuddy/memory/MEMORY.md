# demo（达梦 → MongoDB 迁移工具）· 长期笔记

## 交付形态
现场只拿到二进制 + 配置文件 + 文档，**没有源码**。因此：
- `demo/README.md` = 现场手册（纯运维视角）；`demo/DEVELOP.md` = 开发文档。
  往 README 里写 `go build` / `syncbase` / `GOOS` 这类内容等于给现场添乱。
- 交付目录是 `demo/迁移/`（曾打包成 `迁移.rar`），任何改动都要同步过去。
- 现场文档里凡涉及"实际输出长什么样"的描述，**必须实跑一遍抓真实输出再写**，
  不要凭印象写（曾经把完成汇总的输出格式写错）。

## 达梦连接的硬约束
- **DSN 必须字面量拼接，绝不能用 `url.URL`**：达梦驱动的 `parseDSN`
  （`gorm-dameng/dm8/n.go`）只做字符串切分、不做 URL 解码，而 `url.URL.String()`
  会转义密码里的 `@ ? : # %` 空格，现象是「密码正确却报用户名或密码错误」。
- **`user_status`（用户表）、`sort_no`（组织表）必须写成字符串**：达梦侧是 INT，
  但 `pkg/model/identity` 里对应字段是 string，写数字会让应用侧反序列化失败。
- **`linkeman_email` → `linke_man_email`**（目标集合字段少一个 e）。
- 分页读取用 `ROWNUM` 且无 `ORDER BY`，所以 `-limit N` 取的不是稳定的一批，不能当抽样样本。
- `SafeDSN` / `SafeURI` 必须始终脱敏；且**不能用字符串替换来脱敏**——
  用户名与密码相同时（SYSDBA/SYSDBA）会替换错位置、漏出明文。

## 环境
- 测试达梦：`10.8.19.133:5236`（SYSDBA/SYSDBA），源表 `DLPDATA.did_system_sync_user`（500 条）、
  `DLPDATA.did_system_sync_org`（300 条），由 `demo/_seed` 造出，含 NULL / 中文 / 引号等边界数据。
- 测试 Mongo：`10.8.27.151:32017`（root），库 `identity`。
- `demo/_seed`、`demo/_verify` 以下划线开头，Go 工具链会自动跳过，不影响 `go build ./demo/...`。
