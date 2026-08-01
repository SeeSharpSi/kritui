package kritui_db

import "encoding/json"

func encodeToolNames(names []string) (string, error) {
	if names == nil {
		names = []string{}
	}
	encoded, err := json.Marshal(names)
	return string(encoded), err
}

func decodeToolNames(encoded string) ([]string, error) {
	var names []string
	if err := json.Unmarshal([]byte(encoded), &names); err != nil {
		return nil, err
	}
	if names == nil {
		names = []string{}
	}
	return names, nil
}
