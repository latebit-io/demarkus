package token

import "testing"

func FuzzParseBytes(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("[tokens]\n\"sha256-ab\" = { paths = [\"/*\"], operations = [\"read\"] }\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := ParseBytes(data)
		if err == nil && file.Tokens == nil {
			t.Fatal("nil Tokens on success")
		}
	})
}
