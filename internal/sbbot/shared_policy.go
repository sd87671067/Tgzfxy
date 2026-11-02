package sbbot

import (
	"bytes"
	"encoding/json"
	"slices"
)

func managedQuotaRule(r Doc) bool {
	if str(r["type"]) != "logical" || str(r["mode"]) != "and" || str(r["action"]) != "reject" || str(r["method"]) != "default" || len(r) != 5 || len(list(r["rules"])) != 1 {
		return false
	}
	scope := obj(list(r["rules"])[0])
	return len(stringsOf(scope["inbound"])) == 1 && (len(scope) == 1 || len(scope) == 2 && len(stringsOf(scope["auth_user"])) == 1)
}
func setQuotaBlocks(d Doc, target map[string]bool) {
	if len(target) == 0 {
		owned := false
		for _, v := range list(obj(d["route"])["rules"]) {
			if managedQuotaRule(obj(v)) {
				owned = true
			}
		}
		if !owned {
			return
		}
	}
	route := nested(d, "route")
	rules := []any{}
	keys := []string{}
	for key, v := range target {
		if v {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		scope := Doc{"inbound": []string{key}}
		if u, ok := findUser(d, key); ok {
			scope = Doc{"inbound": []string{u.Inbound}, "auth_user": []string{u.Name}}
		}
		rules = append(rules, Doc{"type": "logical", "mode": "and", "rules": []any{scope}, "action": "reject", "method": "default"})
	}
	for _, v := range list(route["rules"]) {
		if !managedQuotaRule(obj(v)) {
			rules = append(rules, v)
		}
	}
	route["rules"] = rules
}
func setManualPause(d Doc, key string, blocked bool) {
	u, ok := findUser(d, key)
	if !ok {
		target := Blocked(d)
		target[key] = blocked
		SetBlockedDoc(d, target)
		return
	}
	route := nested(d, "route")
	rules := []any{}
	for _, v := range list(route["rules"]) {
		r := obj(v)
		simple := len(r) == 3 || len(r) == 4 && str(r["method"]) == "default"
		if simple && str(r["action"]) == "reject" && ruleForUser(r, u) {
			continue
		}
		rules = append(rules, v)
	}
	if blocked {
		rules = append([]any{Doc{"inbound": []string{u.Inbound}, "auth_user": []string{u.Name}, "action": "reject", "method": "default"}}, rules...)
	}
	route["rules"] = rules
}
func originalVersion(d Doc) string {
	raw, e := baseline(d, "")
	if e != nil {
		return ""
	}
	var original Doc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.Decode(&original)
	return configVersion(original)
}
