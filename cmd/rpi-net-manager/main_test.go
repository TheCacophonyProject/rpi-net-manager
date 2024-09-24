package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaults(t *testing.T) {
	output := `
 AP[1].IN-USE: 
     AP[1].BSSID:70:A7:41:DC:5D:21
AP[2].IN-USE: 
AP[2].BSSID:72:A7:41:9C:5D:21
AP[3].IN-USE: 
AP[3].BSSID:72:A7:41:AC:5D:21
AP[4].IN-USE: 
AP[4].BSSID:60:22:32:40:28:19
    AP[5].IN-USE:*
AP[5].BSSID:70:A7:41:DC:64:21
AP[6].IN-USE: 
AP[6].BSSID:36:A4:3C:4F:0A:E4
`

	bssid, err := getActiveBSSIDFromOutput(output)
	assert.NoError(t, err)
	assert.Equal(t, "70:A7:41:DC:64:21", bssid)

	output2 := `
 AP[1].IN-USE: 
     AP[1].BSSID:70:A7:41:DC:5D:21
AP[2].IN-USE: 
AP[2].BSSID:72:A7:41:9C:5D:21
AP[3].IN-USE: 
AP[3].BSSID:72:A7:41:AC:5D:21
AP[4].IN-USE: 
AP[4].BSSID:60:22:32:40:28:19
AP[5].IN-USE:
AP[5].BSSID:70:A7:41:DC:64:21
AP[6].IN-USE: 
AP[6].BSSID:36:A4:3C:4F:0A:E4
`

	_, err = getActiveBSSIDFromOutput(output2)
	assert.Error(t, err)
}
