package contentquery

import "testing"

func TestParseCutsAtTheNextColumnEvenWhenAValueLooksLikeAPair(t *testing.T) {
	text := "Row: 0 _id=1, address=+12025550100, body=hi, type=2 sneaky, date=5, type=1\n" +
		"Row: 1 _id=2, address=NULL, body=two\nlines, date=6, type=2\n" +
		"Row: 2 _id=3, address=555, body=x, date=7, type=1\n"
	rows, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].Get("body") != "hi, type=2 sneaky" || rows[0].Get("type") != "1" {
		t.Errorf("row 0 = %v", rows[0])
	}
	if _, set := rows[1]["address"]; set || rows[1].Get("body") != "two\nlines" {
		t.Errorf("row 1 = %v", rows[1])
	}
}

func TestParseBlocksKeepsEachMessagesRows(t *testing.T) {
	text := "== msg 7\nRow: 0 _id=1, msg_id=7, address=+12025550100, type=137\nRow: 1 _id=2, msg_id=7, address=insert-address-token, type=151\n== msg 8\nRow: 0 _id=3, msg_id=8, address=+12025550101, type=137\n"
	blocks, err := ParseBlocks(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks["7"]) != 2 || len(blocks["8"]) != 1 || blocks["8"][0].Get("address") != "+12025550101" {
		t.Errorf("blocks = %v", blocks)
	}
}
