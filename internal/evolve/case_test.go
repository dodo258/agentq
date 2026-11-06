package evolve

import (
	"strings"
	"testing"
	"testing/fstest"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func validFS() fstest.MapFS {
	return fstest.MapFS{
		"001-impl-go/case.json":               file(`{"domain":"backend","kind":"task","request":"做一件事","expect":{"merged":true}}`),
		"001-impl-go/_project/.agentq/verify": file("#!/bin/sh\n"),
		"002-skill/case.json":                 file(`{"domain":"install","kind":"program"}`),
		"002-skill/check.sh":                  file("exit 0\n"),
		"_fake/claude.sh":                     file("不是题"),
		"README.md":                           file("不是题"),
	}
}

func TestLoadCases(t *testing.T) {
	cases, err := LoadCases(validFS(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 || cases[0].ID != "001-impl-go" || cases[0].Short() != "impl-go" || cases[1].Kind != KindProgram || cases[0].Held {
		t.Fatalf("%+v", cases)
	}
	held, err := LoadCases(validFS(), true)
	if err != nil || !held[0].Held {
		t.Fatalf("%v %+v", err, held)
	}
}

func TestLoadCaseRejects(t *testing.T) {
	cases := map[string]struct {
		fsys fstest.MapFS
		want string
	}{
		"目录名":     {fstest.MapFS{"impl/case.json": file(`{}`)}, "题号-短名"},
		"没有声明":    {fstest.MapFS{"001-a/x": file("")}, "读 case.json"},
		"JSON 坏了": {fstest.MapFS{"001-a/case.json": file(`{`)}, "解析 case.json"},
		"拼错期望":    {fstest.MapFS{"001-a/case.json": file(`{"domain":"d","kind":"program","expect":{"mergd":true}}`), "001-a/check.sh": file("")}, "mergd"},
		"没领域":     {fstest.MapFS{"001-a/case.json": file(`{"kind":"program"}`), "001-a/check.sh": file("")}, "没写 domain"},
		"未知种类":    {fstest.MapFS{"001-a/case.json": file(`{"domain":"d","kind":"x"}`)}, "不认识"},
		"程序题没脚本":  {fstest.MapFS{"001-a/case.json": file(`{"domain":"d","kind":"program"}`)}, "check.sh"},
		"模型题没门":   {fstest.MapFS{"001-a/case.json": file(`{"domain":"d","kind":"task","request":"r","expect":{"merged":true}}`)}, ".agentq/verify"},
		"没需求":     {fstest.MapFS{"001-a/case.json": file(`{"domain":"d","kind":"new","expect":{"merged":true}}`)}, "没写 request"},
		"需求两行":    {fstest.MapFS{"001-a/case.json": file(`{"domain":"d","kind":"new","request":"a\nb","expect":{"merged":true}}`)}, "只能一行"},
		"什么都算对":   {fstest.MapFS{"001-a/case.json": file(`{"domain":"d","kind":"new","request":"r"}`)}, "什么结果都算对"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCases(tc.fsys, false); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestSelect(t *testing.T) {
	cases, err := LoadCases(validFS(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"impl-go", "001", "001-impl-go"} {
		got, err := Select(cases, []string{name})
		if err != nil || len(got) != 1 || got[0].ID != "001-impl-go" {
			t.Errorf("%s：%v %+v", name, err, got)
		}
	}
	if got, err := Select(cases, nil); err != nil || len(got) != 2 {
		t.Errorf("%v %+v", err, got)
	}
	if got, err := Select(cases, []string{"impl-go", "001"}); err != nil || len(got) != 1 {
		t.Errorf("同一题写两次只跑一次：%v %+v", err, got)
	}
	if _, err := Select(cases, []string{"nope"}); err == nil || !strings.Contains(err.Error(), "没有这道题") {
		t.Errorf("%v", err)
	}
	dup := append(cases[:1:1], Case{ID: "009-impl-go"})
	if _, err := Select(dup, []string{"impl-go"}); err == nil || !strings.Contains(err.Error(), "写全名") {
		t.Errorf("%v", err)
	}
}
