package app

import (
	"io"
	"reflect"
	"testing"
	"testing/fstest"
	"time"

	"agentq/internal/config"
	"agentq/internal/env"
)

// Wire 之后每个模块实例都在，模块的依赖字段和钩子都接上了，Env 只有一份。少接一个，运行到那条路才崩。
func TestWireFillsEverything(t *testing.T) {
	e := &env.Env{Home: t.TempDir(), Cfg: config.Default(), Now: time.Now, Out: io.Discard}
	assets := Assets{RoutineText: map[string]string{config.KindNight: "night", config.KindReview: "review"}, Install: fstest.MapFS{}}
	a := Wire(&App{Env: e}, assets)
	v := reflect.ValueOf(a).Elem()
	envType := reflect.TypeOf(e)
	for i := range v.NumField() {
		f, mod := v.Type().Field(i), v.Field(i)
		if f.Type == envType {
			continue
		}
		if mod.IsNil() {
			t.Errorf("App.%s 没建", f.Name)
			continue
		}
		for k := range mod.Elem().NumField() {
			df, dep := mod.Elem().Type().Field(k), mod.Elem().Field(k)
			switch {
			case df.Type == envType:
				if dep.Interface() != any(e) {
					t.Errorf("%s.Env 不是同一份", f.Name)
				}
			case df.Type.Kind() == reflect.Pointer && df.Type.Elem().Name() == "Service":
				if dep.IsNil() {
					t.Errorf("%s.%s 没接", f.Name, df.Name)
				}
			}
		}
	}
	h := reflect.ValueOf(a.Runner.Hooks)
	for i := range h.NumField() {
		if h.Field(i).Kind() == reflect.Func && h.Field(i).IsNil() {
			t.Errorf("Hooks.%s 没接", h.Type().Field(i).Name)
		}
	}
	if a.Routine.RoutineText[config.KindNight] != "night" || a.Onboard.Assets == nil || a.Notify.MorningIssues == nil {
		t.Errorf("注入的资产或早报钩子没接上")
	}
}
