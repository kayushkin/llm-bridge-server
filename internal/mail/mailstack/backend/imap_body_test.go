package backend

import (
	"strings"
	"testing"
)

func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

func TestDecodeMessageBodiesSplitsAlternativeAndUndoesQuotedPrintable(t *testing.T) {
	raw := crlf(`From: a@example.com
Subject: On my family
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="651d2b26"

--651d2b26
Content-Type: text/plain; charset="utf-8"
Content-Transfer-Encoding: quoted-printable

View this post on the web at https://selectivecontrarianism.substack.com/p/=
on-my-family
her father=E2=80=99s farm.
--651d2b26
Content-Type: text/html; charset="utf-8"
Content-Transfer-Encoding: quoted-printable

<p class=3D"x">her father=E2=80=99s farm.</p>
--651d2b26--
`)
	bodyText, bodyHTML, err := decodeMessageBodies(raw)
	if err != nil {
		t.Fatal(err)
	}
	wantText := "View this post on the web at https://selectivecontrarianism.substack.com/p/on-my-family\r\nher father’s farm."
	if bodyText != wantText {
		t.Fatalf("text:\n got %q\nwant %q", bodyText, wantText)
	}
	if bodyHTML != `<p class="x">her father`+"’"+`s farm.</p>` {
		t.Fatalf("html: %q", bodyHTML)
	}
}

func TestDecodeMessageBodiesWalksNestedPartsSkipsAttachmentsAndConvertsCharset(t *testing.T) {
	raw := crlf(`From: a@example.com
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: text/plain; name="notes.txt"
Content-Disposition: attachment; filename="notes.txt"

attachment text, not the body
--outer
Content-Type: multipart/alternative; boundary="inner"

--inner
Content-Type: text/plain; charset="iso-8859-1"
Content-Transfer-Encoding: base64

Y2Fm6Q==
--inner
Content-Type: text/html; charset="utf-8"
Content-Transfer-Encoding: base64

PGI+Y2Fmw6k8L2I+
--inner--
--outer--
`)
	bodyText, bodyHTML, err := decodeMessageBodies(raw)
	if err != nil {
		t.Fatal(err)
	}
	if bodyText != "café" || bodyHTML != "<b>café</b>" {
		t.Fatalf("text %q html %q", bodyText, bodyHTML)
	}
}

func TestDecodeMessageBodiesReadsASinglePartMessageWithNoContentType(t *testing.T) {
	bodyText, bodyHTML, err := decodeMessageBodies(crlf("From: a@example.com\n\nplain body\n"))
	if err != nil {
		t.Fatal(err)
	}
	if bodyText != "plain body\r\n" || bodyHTML != "" {
		t.Fatalf("text %q html %q", bodyText, bodyHTML)
	}
}
