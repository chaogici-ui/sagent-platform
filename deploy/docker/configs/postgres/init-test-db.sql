-- 单测库：宿主 `go test` 用（目录库已全面 PG，单测载体同源）。
-- 每用例在其中开独立 schema（testpg.Provision），用例间互不可见。
-- 仅在 pg-data 卷为空（首次初始化）时由 entrypoint 执行；存量卷需手工建一次，见 .env.example。
CREATE DATABASE sagent_test OWNER sagent;