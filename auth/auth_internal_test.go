package auth

import "testing"

// 升级前写入的记录没有 kind,解析时必须按用户会话补齐,否则已登录用户在
// Kind(c) 下会变成未知身份。
func TestParseRecordKind(t *testing.T) {
	rec, err := parseRecord(`{"data":{"uid":1},"peer":"rt","exp":0}`)
	if err != nil || rec.Kind != KindUser {
		t.Fatalf("legacy record: kind = %q, err = %v; want %q", rec.Kind, err, KindUser)
	}
	raw, err := recordJSON([]byte(`{"uid":1}`), "rt", KindClient, 0)
	if err != nil {
		t.Fatalf("recordJSON: %v", err)
	}
	if rec, err = parseRecord(raw); err != nil || rec.Kind != KindClient || rec.Peer != "rt" || string(rec.Data) != `{"uid":1}` {
		t.Fatalf("client record: %+v, %v; want kind %q carried through", rec, err, KindClient)
	}
}
