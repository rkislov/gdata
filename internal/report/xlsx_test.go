// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package report

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestXLSXUsesEscapedStringsInsteadOfFormulas(t *testing.T) {
	b, e := XLSX([][]string{{"Задание", "Статус"}, {"=HYPERLINK(\"bad\")", "<успешно & проверено>"}})
	if e != nil {
		t.Fatal(e)
	}
	z, e := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, f := range z.File {
		r, e := f.Open()
		if e != nil {
			t.Fatal(e)
		}
		v, e := io.ReadAll(r)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
		d := xml.NewDecoder(bytes.NewReader(v))
		for {
			_, e = d.Token()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatalf("invalid XML %s: %v", f.Name, e)
			}
		}
		if f.Name == "xl/worksheets/sheet1.xml" {
			found = true
			if strings.Contains(string(v), "<f>") || !strings.Contains(string(v), "&lt;успешно &amp; проверено&gt;") {
				t.Fatal("unsafe or incorrectly escaped cells")
			}
		}
	}
	if !found {
		t.Fatal("sheet absent")
	}
}
func TestCSVFormulaGuard(t *testing.T) {
	for _, v := range []string{"=1+1", " +cmd", "@SUM(A1)", "\t-1"} {
		if !strings.HasPrefix(CSVCell(v), "'") {
			t.Fatalf("unguarded value %q", v)
		}
	}
	if CSVCell("ordinary") != "ordinary" {
		t.Fatal("ordinary text modified")
	}
}
func TestExcelColumnNames(t *testing.T) {
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 701: "ZZ", 702: "AAA"} {
		if got := column(i); got != want {
			t.Errorf("%d: %s != %s", i, got, want)
		}
	}
}
