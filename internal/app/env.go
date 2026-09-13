package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// envPrefix names every environment variable this binary reads, so a
// container can be configured purely through its environment (see
// docs/configuration.md).
const envPrefix = "SCOTMESH_CHAT_"

// applyEnv overlays configuration from SCOTMESH_CHAT_* environment
// variables named in environ (normally os.Environ()), on top of whatever
// Load already decoded from defaults and the TOML file. It is as strict as
// the TOML file: a SCOTMESH_CHAT_* variable that doesn't name a known
// setting is an error, so a typo can't silently leave a setting at its
// default.
func applyEnv(cfg *Config, environ []string) error {
	fields := map[string]reflect.Value{}
	collectFields(reflect.ValueOf(cfg).Elem(), "", fields)

	var errs []error
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, envPrefix) {
			continue
		}
		field, ok := fields[strings.TrimPrefix(name, envPrefix)]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: no such setting", name))
			continue
		}
		if err := setField(field, value); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// collectFields maps every leaf Config field to the environment variable
// name derived from its "toml" tag (dotted paths joined with "_", upper
// cased), so the TOML file and the environment can't drift apart. A nested
// settings group (Hub, Group, Page, RRC) is walked; a slice, even of
// structs, is a leaf, set as one value (see setField).
func collectFields(v reflect.Value, prefix string, out map[string]reflect.Value) {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		name := prefix + tag
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct && fv.Type() != reflect.TypeFor[Duration]() {
			collectFields(fv, name+"_", out)
			continue
		}
		out[strings.ToUpper(name)] = fv
	}
}

// setField parses raw into field, whose type decides how: a Duration takes
// a Go duration string, a []string takes a JSON array or (with no leading
// "[") a comma-separated list, and a slice of anything else (RoomConfig)
// takes a JSON array of objects.
func setField(field reflect.Value, raw string) error {
	if field.Type() == reflect.TypeFor[Duration]() {
		var d Duration
		if err := d.UnmarshalText([]byte(raw)); err != nil {
			return err
		}
		field.Set(reflect.ValueOf(d))
		return nil
	}
	switch field.Kind() {
	case reflect.String:
		field.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("not true or false: %w", err)
		}
		field.SetBool(b)
	case reflect.Int:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return err
		}
		field.SetInt(int64(n))
	case reflect.Slice:
		return setSlice(field, raw)
	default:
		return fmt.Errorf("unsupported setting type %s", field.Type())
	}
	return nil
}

func setSlice(field reflect.Value, raw string) error {
	trimmed := strings.TrimSpace(raw)
	if field.Type().Elem().Kind() == reflect.String && !strings.HasPrefix(trimmed, "[") {
		var list []string
		if trimmed != "" {
			for _, s := range strings.Split(raw, ",") {
				list = append(list, strings.TrimSpace(s))
			}
		}
		field.Set(reflect.ValueOf(list))
		return nil
	}
	ptr := reflect.New(field.Type())
	if err := json.Unmarshal([]byte(raw), ptr.Interface()); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	field.Set(ptr.Elem())
	return nil
}
