package main

import "testing"

// VM /api/v1/query 的原始返回：value[1] 是字符串数字。解析必须容忍
// 非法条目（跳过而不是整批失败）——一条脏样本不该让整页资源状态变"未知"
const vmMetricsBody = `{"status":"success","data":{"resultType":"vector","result":[
  {"metric":{"resource_id":"ip-10-10-0-20","instance":"10.10.0.20:19090"},"value":[1790069384,"502"]},
  {"metric":{"resource_id":"order.prod.host.sagent-1","instance":"sagent-1:19090"},"value":[1790069384,"537"]},
  {"metric":{"instance":"nowhere:19090"},"value":[1790069384,"9"]},
  {"metric":{"resource_id":"bad","instance":"bad:1"},"value":[1790069384,"not-a-number"]}
]}}`

const vmFreshnessBody = `{"status":"success","data":{"resultType":"vector","result":[
  {"metric":{"resource_id":"ip-10-10-0-20","instance":"10.10.0.20:19090"},"value":[1790069384,"13.4"]},
  {"metric":{"resource_id":"order.prod.host.sagent-1","instance":"sagent-1:19090"},"value":[1790069384,"900.5"]}
]}}`

func TestParseMetricsRows(t *testing.T) {
	rows, err := parseVMRows([]byte(vmMetricsBody))
	if err != nil {
		t.Fatalf("parseVMRows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("非法数字应跳过，其余保留（含只有 instance 的行）：%+v", rows)
	}
	byID := map[string]vmRow{}
	for _, r := range rows {
		byID[r.ResourceID] = r
	}
	if r := byID["ip-10-10-0-20"]; r.Value != 502 || r.Instance != "10.10.0.20:19090" {
		t.Fatalf("指标条数/实例解析错：%+v", r)
	}
	if _, ok := byID["bad"]; ok {
		t.Fatalf("非法数字应跳过：%+v", rows)
	}
}

func TestParseVMRowsBadBody(t *testing.T) {
	if _, err := parseVMRows([]byte("<html>502</html>")); err == nil {
		t.Fatal("非 JSON 响应必须报错（VM 不可达时要能和'零指标'区分开）")
	}
}

// mergeResourceStats：两路查询按「resource_id 优先、instance 兜底」对齐。
// 只有一行数据（如只有指标数没年龄）也要保留，年龄未知记 -1
func TestMergeResourceStats(t *testing.T) {
	metrics := []vmRow{
		{ResourceID: "ip-10-10-0-20", Instance: "10.10.0.20:19090", Value: 502},
		{ResourceID: "order.prod.host.sagent-1", Instance: "sagent-1:19090", Value: 0},
		{Instance: "only-instance:19090", Value: 7},
	}
	age := []vmRow{
		{ResourceID: "ip-10-10-0-20", Instance: "10.10.0.20:19090", Value: 13.4},
		{Instance: "only-instance:19090", Value: 4},
	}
	rows := mergeResourceStats(metrics, age)
	byID := map[string]resourceStat{}
	for _, r := range rows {
		byID[r.ResourceID] = r
	}
	if len(rows) != 3 {
		t.Fatalf("合并后应保留 3 行：%+v", rows)
	}
	if r := byID["ip-10-10-0-20"]; r.Metrics != 502 || !r.Reporting || r.AgeSec != 13.4 || r.Instance != "10.10.0.20:19090" {
		t.Fatalf("采集中资源应 reporting=true 且带 instance：%+v", r)
	}
	if r := byID["order.prod.host.sagent-1"]; r.Reporting || r.AgeSec != -1 {
		t.Fatalf("零指标资源不应标为在报，年龄未知用 -1：%+v", r)
	}
	if r := byID["only-instance:19090"]; r.Metrics != 7 || r.AgeSec != 4 {
		t.Fatalf("只有 instance 的行也要能按 instance 对齐：%+v", r)
	}
}
