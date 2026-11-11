package panel

import "testing"

// 导入必须保留文件里的 realm。样例导出（cockpit 账号详情）带 auth.realm，此前被丢弃、
// 只按 domain 反推：domain 缺失或是通用域名时，国际版号会被导成国内版，之后所有上游
// 请求打错端点、积分也记错域。这一组用例钉住「三种形态都读得到 realm」+「脏值回退」。

func TestDecodeCockpitAccountsKeepsRealm(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "nested camel",
			in:   `[{"account":{"uid":"u1"},"auth":{"accessToken":"a","refreshToken":"r","domain":"www.codebuddy.cn","realm":"global"}}]`,
			want: "global",
		},
		{
			name: "single nested",
			in:   `{"account":{"uid":"u2"},"auth":{"accessToken":"a","refreshToken":"r","domain":"www.workbuddy.ai","realm":"cn"}}`,
			want: "cn",
		},
		{
			name: "flat camel",
			in:   `{"uid":"u3","accessToken":"a","refreshToken":"r","realm":"global"}`,
			want: "global",
		},
		{
			name: "flat snake",
			in:   `[{"uid":"u4","access_token":"a","refresh_token":"r","realm":"global"}]`,
			want: "global",
		},
	}
	for _, c := range cases {
		got, err := decodeCockpitAccounts([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", c.name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: got %d accounts, want 1", c.name, len(got))
		}
		if got[0].Realm != c.want {
			t.Fatalf("%s: realm = %q, want %q（文件里写了就以文件为准）", c.name, got[0].Realm, c.want)
		}
	}
}

// importRealm 只认 cn/global，其余（拼错、intl、sg、大小写、空白）一律回退按 domain 推断：
// 脏值写进凭据会被 BackfillRealmFor 拒绝，或让账号在域统计里凭空少一个。
func TestImportRealmNormalization(t *testing.T) {
	cases := []struct {
		explicit, domain, want string
	}{
		{"cn", "api.workbuddy.ai", "cn"},
		{"global", "www.codebuddy.cn", "global"},
		{"  GLOBAL  ", "", "global"},
		{" Intl ", "api.workbuddy.ai", "global"},
		{"", "api.workbuddy.ai", "global"},
		{"", "www.codebuddy.cn", "cn"},
		{"", "", "cn"},
	}
	for _, c := range cases {
		if got := importRealm(c.explicit, c.domain); got != c.want {
			t.Errorf("importRealm(%q, %q) = %q, want %q", c.explicit, c.domain, got, c.want)
		}
	}
}
