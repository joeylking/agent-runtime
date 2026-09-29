package agentrt

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// marshalCanonical is canonicalJSON as it was when the stored hashes were
// written: decode with UseNumber, then json.Marshal, which sorts map keys.
// The explicit encoder must match it byte for byte.
func marshalCanonical(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("null"), nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

var canonicalCorpus = []string{
	``, `  `, `null`, `true`, `false`, `0`, `-0`, `1`, `1.0`, `1e0`, `1E+2`, `-12.5e-3`,
	`9007199254740993`, `123456789012345678901234567890`, `0.1`, `3.141592653589793238462643383279`, `1e400`,
	`""`, `"plain"`, `"quote \" backslash \\ slash \/"`, `"\b\f\n\r\t"`, `"\u0000\u0001\u001f\u007f"`,
	`"<script>&amp;</script>"`, `"\u003c\u003e\u0026"`, `"é ü 日本語 🙂"`, `"\ud83d\ude42"`, `"\u2028\u2029"`,
	`"\ud800"`, `"\udc00x"`, "\"\xff\xfe\"", "\"a\xc3\"", `"\u00e9"`, "\"\u2028 raw\"",
	`[]`, `{}`, `[[]]`, `{"":{}}`, `[1,"a",null,true,{"b":[]}]`,
	`{"b":1,"a":2}`, `{"B":1,"a":2,"_":3,"é":4,"e":5,"😀":6,"日":7}`, `{"a":{"z":1,"y":{"x":[{"w":0,"v":1}]}}}`,
	`{"<":"&","key with space":"v"}`, `{"a":2,"a":1}`, `{"n":1.50,"m":[1e5,2E-5,-0.0]}`,
	` { "spaced" : [ 1 , 2 ] } `, `{"a":1} trailing`, `{"tool":"push","args":{"n":7}}`,
	`{"html":"<a href=\"x\">&</a>","ctl":"\u0007","tab":"\t"}`,
}

// Byte-identity with encoding/json over the corpus and over random
// documents. If this fails for some input class, the stored hashes of
// that class would change: stop and decide, do not update the encoder.
func TestCanonicalJSON_MatchesEncodingJSON(t *testing.T) {
	check := func(raw string) {
		t.Helper()
		want, werr := marshalCanonical(json.RawMessage(raw))
		got, gerr := canonicalJSON(json.RawMessage(raw))
		if (werr != nil) != (gerr != nil) {
			t.Fatalf("%q: error mismatch: %v vs %v", raw, werr, gerr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%q:\n got %s\nwant %s", raw, got, want)
		}
	}
	for _, raw := range canonicalCorpus {
		check(raw)
	}
	seed := time.Now().UnixNano()
	r := rand.New(rand.NewSource(seed))
	for i := 0; i < 5000; i++ {
		v := randomValue(r, 4)
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		check(string(b))
		// The same document with every string's escapes spelled out.
		check(strings.ReplaceAll(string(b), `\u003c`, `<`))
	}
	t.Logf("seed %d", seed)
}

func randomValue(r *rand.Rand, depth int) any {
	n := r.Intn(8)
	if depth == 0 && n >= 6 {
		n = r.Intn(6)
	}
	switch n {
	case 0:
		return nil
	case 1:
		return r.Intn(2) == 0
	case 2:
		nums := []string{"0", "-1", "42", "1.5", "1.0", "1e21", "9007199254740993", "-0.000001", "2E-7", "123456789.123456789"}
		return json.Number(nums[r.Intn(len(nums))])
	case 3, 4:
		return randomString(r)
	case 6:
		out := make([]any, r.Intn(4))
		for i := range out {
			out[i] = randomValue(r, depth-1)
		}
		return out
	default:
		out := map[string]any{}
		for i := r.Intn(5); i > 0; i-- {
			out[randomString(r)] = randomValue(r, depth-1)
		}
		return out
	}
}

func randomString(r *rand.Rand) string {
	alphabet := []string{"a", "Z", "0", " ", "\"", "\\", "/", "<", ">", "&", "\n", "\t", "\b", "\f", "\r", "\x00", "\x1f", "\x7f", "é", "日", "🙂", "\u2028", "\u2029", "\ufffd", "\xff"}
	var b strings.Builder
	for i := r.Intn(8); i > 0; i-- {
		b.WriteString(alphabet[r.Intn(len(alphabet))])
	}
	return b.String()
}

// Hashes computed with the json.Marshal implementation and pinned, so
// that neither this encoder nor a Go release can move a stored approval
// hash or observation hash without a failing test.
func TestContentHash_Golden(t *testing.T) {
	golden := map[string]string{
		"":                                 "74234e98afe7498fb5daf1f36ac2d78acc339464f950703b8c019892f982b90b",
		"  ":                               "74234e98afe7498fb5daf1f36ac2d78acc339464f950703b8c019892f982b90b",
		"null":                             "74234e98afe7498fb5daf1f36ac2d78acc339464f950703b8c019892f982b90b",
		"true":                             "b5bea41b6c623f7c09f1bf24dcae58ebab3c0cdd90ad966bc43a45b44867e12b",
		"false":                            "fcbcf165908dd18a9e49f7ff27810176db8e9f63b4352213741664245224f8aa",
		"0":                                "5feceb66ffc86f38d952786c6d696c79c2dbc239dd4e91b46729d73a27fb57e9",
		"-0":                               "ed79f26d03f412bde3db206601a698e0bb451bea9e3cc25289636ac17ea74b0a",
		"1":                                "6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b",
		"1.0":                              "d0ff5974b6aa52cf562bea5921840c032a860a91a3512f7fe8f768f6bbe005f6",
		"1e0":                              "bcac40fbed4ed4e1042a0c9dafbe9cdd58447415f19a11e59add45fdc083e2f5",
		"1E+2":                             "c7edae06d1671afc40e76b33cf2920bb1611c50cb5b6fdc07aa485885a834efe",
		"-12.5e-3":                         "19a67ac5740420a4baf52a2b7d3e902cf5f1b1e7752c56a7faa8e1ccf8006768",
		"9007199254740993":                 "a1c367c29158357e62a3ff5d3e800fb7698a22396439dbc0a9d4929322afd35d",
		"123456789012345678901234567890":   "f54e5c8f810648e7638d25eb7ed6d24b7e5999d588e88826f2aa837d2ee52ecd",
		"0.1":                              "14be4b45f18e0d8c67b4f719b5144eee88497e413709d11d85b096d8e2346310",
		"3.141592653589793238462643383279": "27f6ef06c05a66675cc7f80fca76488b57496bfa62c362128194da38905f2a76",
		"1e400":                            "f2bba4568fecd4b9729970732e571ac9373a33fb2d6a960794a41f0f2ecdbc25",
		"\"\"":                             "12ae32cb1ec02d01eda3581b127c1fee3b0dc53572ed6baf239721a03d82e126",
		"\"plain\"":                        "945603a8f587786b463c3f94fce115c0fae88fac2728cc96ddf5981cf7f61741",
		"\"quote \\\" backslash \\\\ slash \\/\"": "0bf8d8cac25a2ba37fc6ddcfeb6e43572799b87ebf390873fd7a7dd22447b0a1",
		"\"\\b\\f\\n\\r\\t\"":                     "88821412381596c0dc2c66e310ae7e66100d7e4196456e2e9f3d261d0b562746",
		"\"\\u0000\\u0001\\u001f\\u007f\"":        "3a5f25746de9e33a0a1d8d04f3bb8e3cc03c04b69fbe67fdbc4091e2b3c48fa8",
		"\"<script>&amp;</script>\"":              "82f277f8f0331a2f51b10e04e886175451d160438aef59587053bfbcea064dd7",
		"\"\\u003c\\u003e\\u0026\"":               "386d184405c75e79dd1e915ce0109a04c5c4711b874b1c46ec54715bbcdab90f",
		"\"é ü 日本語 🙂\"":                           "d8ecd3a030bab6b820c7861b19f1d8669e63c226513556680edd3af2ac7ca28d",
		"\"\\ud83d\\ude42\"":                      "ec1c0c982d55c84da49d4c52e6458fbaf832ae8cba82b4b8b3f70f43454cb7a2",
		"\"\\u2028\\u2029\"":                      "b36c0076c7e28a52a8a9cb89306b164f887df89d715680d7b1ca8a858a8f3d33",
		"\"\\ud800\"":                             "568601070314e0f4489c9944b3c151d5251d379330008293bb7e1a826a22a845",
		"\"\\udc00x\"":                            "6c48f3826f0e5ad2680ab79ccd83fa555ad9069de70bb0f3b646c8b838cc9513",
		"\"\xff\xfe\"":                            "c2307176792dac65f571010f041ad4b51fa22b1e20f55d8a7890d10b6a42258c",
		"\"a\xc3\"":                               "53629dfae068d0fb8ca6d7c423de50627d7fc0b226a67fc93f33b490382b4d1a",
		"\"\\u00e9\"":                             "f2886017e9c7abacf804b54d64787dce2b611c9544ba21f3affdd126a6e50086",
		"\"\u2028 raw\"":                          "b96b00129d21121b0b7a5823fb30089a8672cedbbd66f192cad302af4d83b69e",
		"[]":                                      "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
		"{}":                                      "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
		"[[]]":                                    "cf1cbb66a638b4860a516671fb74850e6ccf787fe6c4c8d29e9c04efe880bd05",
		"{\"\":{}}":                               "53ed392aab7233d10f2fc1915f8d15da641501c8b04ee20cb89ab851380188ec",
		"[1,\"a\",null,true,{\"b\":[]}]":          "f535a2d063fec68fb2bd1c001e497de0eec6a97884fd31891961e15028812dba",
		"{\"b\":1,\"a\":2}":                       "d3626ac30a87e6f7a6428233b3c68299976865fa5508e4267c5415c76af7a772",
		"{\"B\":1,\"a\":2,\"_\":3,\"é\":4,\"e\":5,\"😀\":6,\"日\":7}":                  "4dc6ac2731156b6b9a580eaa1105ee09f002b1a60c662c90bdad9b27cea5308f",
		"{\"a\":{\"z\":1,\"y\":{\"x\":[{\"w\":0,\"v\":1}]}}}":                        "ef2ae99f38d6a27bb5eedf5a97c295f84e764ec9347a3c2a8620184c426b0b12",
		"{\"<\":\"&\",\"key with space\":\"v\"}":                                     "ac84aa4594d83b01ce4097b6c067e8eb4a96aafe7171e515df26cbbae8638299",
		"{\"a\":2,\"a\":1}":                                                          "015abd7f5cc57a2dd94b7590f04ad8084273905ee33ec5cebeae62276a97f862",
		"{\"n\":1.50,\"m\":[1e5,2E-5,-0.0]}":                                         "99a916ac72d6c6a45b638603e2b3385d14a8bd7df100584bb68aaf3485b33323",
		" { \"spaced\" : [ 1 , 2 ] } ":                                               "d69e48ab4986dbdd32f787657215ebcf63de392b191179f29385e74f55c20c49",
		"{\"a\":1} trailing":                                                         "015abd7f5cc57a2dd94b7590f04ad8084273905ee33ec5cebeae62276a97f862",
		"{\"tool\":\"push\",\"args\":{\"n\":7}}":                                     "9772f8a7199f50b80af49123800a5c7a49e3d97b35a3444f4aa482963cf04333",
		"{\"html\":\"<a href=\\\"x\\\">&</a>\",\"ctl\":\"\\u0007\",\"tab\":\"\\t\"}": "b5563be8cc8ab5d70e6a8a412dcf9eabf6ad610d9088332dc25ee2fac54dea26",
	}
	for _, raw := range canonicalCorpus {
		if got := contentHash(json.RawMessage(raw)); got != golden[raw] {
			t.Errorf("contentHash(%q) = %s, want %s", raw, got, golden[raw])
		}
	}
	req := ToolRequest{RunID: "r1", StepID: "s1", Spec: ToolSpec{Name: "push", Description: "push <main> & tag", InputSchema: json.RawMessage(`{"type":"object"}`), SideEffect: RemoteMutation, Timeout: 30 * time.Second}, Args: json.RawMessage(`{"n":7,"ref":"refs/heads/main"}`)}
	got := approvalHash("remote_mutation", json.RawMessage(`{"tool":"push","args":{"n":7}}`), json.RawMessage(`{"title":"Publish <PR> & tag","lines":1.0}`), req)
	if want := "175bcfc7a7e3cdab6b98d7b50caf9f2a8e59d5208211b97de39a4b027c7fa0b2"; got != want {
		t.Errorf("approvalHash = %s, want %s", got, want)
	}
}

func TestCheckJSON_RejectsWhatWouldHashLossily(t *testing.T) {
	for _, ok := range []string{`{}`, `[]`, `1`, `"s"`, `{"a":{"a":1},"b":[{"a":1},{"a":2}]}`, `{"a":1,"b":{"a":2}}`, `[{"a":1},{"a":1}]`, `"\ud83d\ude42"`, `{"é":"日本"}`} {
		if err := checkJSON(json.RawMessage(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{`{"a":`, `not json`, `{"a":1} {"b":2}`, `{"a":1,"a":2}`, `{"x":{"a":1,"a":1}}`, `[{"k":1,"b":2,"k":3}]`, "\"\xff\"", "{\"\xc3\":1}", ``} {
		if err := checkJSON(json.RawMessage(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
