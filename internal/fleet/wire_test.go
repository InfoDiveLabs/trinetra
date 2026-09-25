package fleet

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
)

func TestBatchRoundTrip(t *testing.T) {
	sd, _ := json.Marshal(SamplesData{TS: 100, Metrics: map[string]float64{"cpu": 12.5}})
	in := []Record{
		{Seq: 1, Kind: KindSamples, TS: 100, Data: sd},
		{Seq: 2, Kind: KindAlert, TS: 101, Data: json.RawMessage(`{"time":101,"key":"cpu"}`)},
	}
	b, err := EncodeBatch(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeBatch(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Seq != 1 || out[1].Kind != KindAlert || string(out[1].Data) != `{"time":101,"key":"cpu"}` {
		t.Fatalf("round trip = %+v", out)
	}
}

func TestDecodeBatchRejectsGzipBomb(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	line := `{"seq":1,"kind":"alert","ts":1,"data":"` + strings.Repeat("A", 1<<20) + `"}` + "\n"
	for i := 0; i < 20; i++ { // ~20 MiB decompressed
		zw.Write([]byte(line))
	}
	zw.Close()
	if _, err := DecodeBatch(&buf); err == nil {
		t.Fatal("oversized batch accepted")
	}
}

func TestDecodeBatchRejectsGarbage(t *testing.T) {
	if _, err := DecodeBatch(strings.NewReader("not gzip")); err == nil {
		t.Fatal("garbage accepted")
	}
}
