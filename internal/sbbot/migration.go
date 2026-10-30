package sbbot

import (
	"errors"
	"fmt"
	"slices"
)

type MigrationUser struct {
	OldTag       string `json:"old_tag"`
	OldPort      int64  `json:"old_port"`
	OldName      string `json:"old_name"`
	OldUUID      string `json:"old_uuid"`
	Outbound     string `json:"outbound"`
	ProposedName string `json:"proposed_name"`
	ProposedUUID string `json:"proposed_uuid"`
	Conflict     bool   `json:"conflict"`
	Impact       string `json:"impact"`
}

// PlanMigration only suggests identities. It never writes SQLite, config, or
// replaces legacy listeners. A distinct identity is needed per old traffic key.
func PlanMigration(d Doc) ([]MigrationUser, error) {
	var plan []MigrationUser
	groups := map[string]int{}
	for _, raw := range list(d["inbounds"]) {
		in := obj(raw)
		if str(in["type"]) != "vless" || isUnified(in) {
			continue
		}
		tag := str(in["tag"])
		out := str(obj(d["route"])["final"])
		for _, raw := range list(obj(d["route"])["rules"]) {
			r := obj(raw)
			if !slices.Contains(stringsOf(r["inbound"]), tag) {
				continue
			}
			if str(r["outbound"]) == ManagedBlock {
				continue
			}
			if len(r) != 2 && !(len(r) == 3 && str(r["action"]) == "route") {
				return nil, errors.New("旧入口存在条件路由，需人工审阅：" + tag)
			}
			out = str(r["outbound"])
			break
		}
		if out == "" || Kind(d, out) != "outbound" {
			return nil, errors.New("无法确定旧出站：" + tag)
		}
		for i, raw := range list(in["users"]) {
			u := obj(raw)
			id, e := canonicalUUID(str(u["uuid"]))
			if e != nil {
				return nil, e
			}
			name := tag
			if len(list(in["users"])) > 1 {
				name = fmt.Sprintf("%s-%d", tag, i+1)
			}
			if !validName(name) {
				return nil, errors.New("迁移用户名需人工选择：" + tag)
			}
			plan = append(plan, MigrationUser{OldTag: tag, OldPort: integer(in["listen_port"]), OldName: str(u["name"]), OldUUID: id, Outbound: out, ProposedName: name, ProposedUUID: id, Impact: "旧端口和 UUID 保留；统一入口新链接需重新导入端口/SNI/公钥；历史额度与到期暂不复制"})
			groups[id]++
		}
	}
	seen := map[string]bool{}
	for i := range plan {
		u := &plan[i]
		u.Conflict = groups[u.OldUUID] > 1
		if u.Conflict && seen[u.OldUUID] {
			id, e := NewUUID()
			if e != nil {
				return nil, e
			}
			u.ProposedUUID = id
			u.Impact = "仅建议为此统一用户生成新 UUID；旧端口/旧 UUID 不变，旧客户端继续可用；需确认新链接、额度与到期策略"
		}
		seen[u.OldUUID] = true
	}
	return plan, nil
}
