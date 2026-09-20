package resource

const (
	TagResourceID     = "resource_id"
	TagResourceType   = "resource_type"
	TagBusinessSystem = "business_system"
	TagEnv            = "env"
	TagIDC            = "idc"
	TagCluster        = "cluster"
)

// Labels 资源标签，统一注入到所有指标中
type Labels struct {
	ResourceID     string
	ResourceType   string
	BusinessSystem string
	Env            string
	IDC            string
	Cluster        string
}

// ToMap 转换为 map[string]string
func (l Labels) ToMap() map[string]string {
	return map[string]string{
		TagResourceID:     l.ResourceID,
		TagResourceType:   l.ResourceType,
		TagBusinessSystem: l.BusinessSystem,
		TagEnv:            l.Env,
		TagIDC:            l.IDC,
		TagCluster:        l.Cluster,
	}
}
