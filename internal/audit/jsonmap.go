package audit

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
)

// JSONMap is a generic JSONB-backed map. The GORM column tag is `jsonb`
// so PostgreSQL stores it natively; on read we unmarshal into a string-
// keyed map.
type JSONMap map[string]any

// Value implements driver.Valuer.
func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return []byte("{}"), nil
	}

	return json.Marshal(m)
}

// Scan implements sql.Scanner.
func (m *JSONMap) Scan(src any) error {
	if src == nil {
		*m = JSONMap{}

		return nil
	}

	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("audit: JSONMap.Scan: unsupported source type")
	}
	if len(b) == 0 {
		*m = JSONMap{}

		return nil
	}

	out := JSONMap{}
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	*m = out

	return nil
}
