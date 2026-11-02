// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"strings"
	"testing"
)

func TestSQLSchemaRewritePreservesDataAndVersionedComments(t *testing.T) {
	source := "/*!50003 CREATE TRIGGER source_db.tr BEFORE UPDATE ON `source_db`.`t` SET @x='source_db.t'; */\nINSERT INTO t VALUES('it\\'s source_db.t',\"source_db.t\"); /* source_db.t */ -- source_db.t\n"
	var out strings.Builder
	if e := rewriteSQLSchema(strings.NewReader(source), &out, "source_db", "restored_db"); e != nil {
		t.Fatal(e)
	}
	want := strings.Replace(source, "source_db.tr", "`restored_db`.tr", 1)
	want = strings.Replace(want, "`source_db`.`t`", "`restored_db`.`t`", 1)
	if out.String() != want {
		t.Fatalf("rewrite changed SQL data: %s", out.String())
	}
}

func TestSQLSchemaRewriteANSIQuotedQualifier(t *testing.T) {
	var out strings.Builder
	input := `CREATE VIEW "source_db"."v" AS SELECT 'source_db', "source_db" FROM "source_db"."t";`
	want := `CREATE VIEW ` + "`restored_db`" + `."v" AS SELECT 'source_db', "source_db" FROM ` + "`restored_db`" + `."t";`
	if e := rewriteSQLSchema(strings.NewReader(input), &out, "source_db", "restored_db"); e != nil || out.String() != want {
		t.Fatal(out.String(), e)
	}
}
