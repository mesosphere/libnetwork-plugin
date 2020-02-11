package driver

import (
	"fmt"
	"strings"
)

func parseDockerOptions(dockerOpt map[string]interface{}, optMap *map[string]interface{}) error {
	var errorMsgs []string

	// This parses both `com.docker.network.generic` options (given as map) and
	// some of the core docker options, such as `com.docker.network.enable_ipv6`
	// assuming they are both the same thing
	for k, v := range dockerOpt {

		// Ensure that key/value format is always in the same format
		kvOpt := map[string]interface{}{k: v}
		if k == "com.docker.network.generic" {
			if v, ok := v.(map[string]interface{}); ok {
				kvOpt = v
			} else {
				errorMsgs = append(errorMsgs,
					fmt.Sprintf("Docker options error: Expecting `com.docker.network.generic` to be a map"))
			}
		}

		// Process remaining options, updating optMap when successful
		for k, v := range kvOpt {
			if dv, ok := (*optMap)[k]; ok {
				switch dv.(type) {
				case bool:
					if v, ok := v.(bool); ok {
						(*optMap)[k] = v
					} else {
						errorMsgs = append(errorMsgs, fmt.Sprintf("Expecting option '%s' to be boolean", k))
					}
					break

				case string:
					if v, ok := v.(string); ok {
						(*optMap)[k] = v
					} else {
						errorMsgs = append(errorMsgs, fmt.Sprintf("Expecting option '%s' to be string", k))
					}
					break
				}
			} else {
				errorMsgs = append(errorMsgs, fmt.Sprintf("Unknown option '%s'", k))
			}
		}
	}

	if len(errorMsgs) > 0 {
		return fmt.Errorf(strings.Join(errorMsgs, ", "))
	}

	return nil
}
