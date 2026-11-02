// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package report

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// XLSX emits inline strings, never formula cells. This keeps untrusted job names inert.
func XLSX(rows [][]string) ([]byte, error) {
	if len(rows) > 1048576 {
		return nil, fmt.Errorf("XLSX row limit exceeded")
	}
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	parts := map[string]string{
		"[Content_Types].xml":        `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`,
		"_rels/.rels":                `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`,
		"xl/workbook.xml":            `<?xml version="1.0" encoding="UTF-8"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Операции" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`,
	}
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels"} {
		w, e := z.Create(name)
		if e != nil {
			return nil, e
		}
		if _, e = io.WriteString(w, parts[name]); e != nil {
			return nil, e
		}
	}
	w, e := z.Create("xl/worksheets/sheet1.xml")
	if e != nil {
		return nil, e
	}
	_, e = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews><cols><col min="1" max="20" width="24" customWidth="1"/></cols><sheetData>`)
	if e != nil {
		return nil, e
	}
	for i, row := range rows {
		if _, e = fmt.Fprintf(w, `<row r="%d">`, i+1); e != nil {
			return nil, e
		}
		for j, value := range row {
			if j >= 16384 || len([]rune(value)) > 32767 {
				return nil, fmt.Errorf("XLSX cell limit exceeded")
			}
			if _, e = fmt.Fprintf(w, `<c r="%s%d" t="inlineStr"><is><t xml:space="preserve">`, column(j), i+1); e != nil {
				return nil, e
			}
			if e = xml.EscapeText(w, []byte(value)); e != nil {
				return nil, e
			}
			if _, e = io.WriteString(w, `</t></is></c>`); e != nil {
				return nil, e
			}
		}
		if _, e = io.WriteString(w, `</row>`); e != nil {
			return nil, e
		}
	}
	if _, e = io.WriteString(w, `</sheetData></worksheet>`); e != nil {
		return nil, e
	}
	if e = z.Close(); e != nil {
		return nil, e
	}
	return out.Bytes(), nil
}
func column(i int) string {
	var s string
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}
func CSVCell(s string) string {
	t := strings.TrimLeft(s, " \t\r\n")
	if t != "" && strings.ContainsRune("=+-@", rune(t[0])) {
		return "'" + s
	}
	return s
}
