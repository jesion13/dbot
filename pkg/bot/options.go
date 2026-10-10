package dbot

import (
	"net/http"
	"reflect"
	"strconv"
	"unsafe"

	"golang.org/x/exp/constraints"
)

// gets value from path value under given key and attempts to convert to given type
func GetPathValue[T constraints.Integer | string](r *http.Request, key string) (T, error) {
	val := r.PathValue(key)
	return CastAs[T](val)
}

// gets value from form value under given key and attempts to convert to given type
// returns no error if value under key is empty string
func GetFormValue[T constraints.Integer | string](r *http.Request, key string) (T, error) {
	var def T
	val := r.FormValue(key)
	if val == "" {
		return def, nil
	}

	return CastAs[T](val)
}

func CastAs[T constraints.Integer | string](val string) (T, error) {
	var t T
	var ret any
	var err error
	switch any(t).(type) {
	case string:
		return any(val).(T), nil

	case uint, uint8, uint16, uint32, uint64:
		// nolint:gosec
		ret, err = strconv.ParseUint(val, 10, int(unsafe.Sizeof(t))*8)

	case int, int8, int16, int32, int64:
		// nolint:gosec
		ret, err = strconv.ParseInt(val, 10, int(unsafe.Sizeof(t))*8)
	}
	if err != nil {
		return t, err
	}

	ret = reflect.ValueOf(ret).Convert(reflect.TypeOf(t)).Interface()

	return ret.(T), nil
}
