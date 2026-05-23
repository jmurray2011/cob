package output

import "github.com/jmurray2011/cob/internal/cob"

// silentRenderer drops every asset-stream event. Picked for JSON mode
// (where streaming progress would corrupt the parseable output) and
// --quiet (where the caller explicitly asked for nothing).
type silentRenderer struct{}

func (silentRenderer) AssetsExpected(int, int64)        {}
func (silentRenderer) AssetStart(string, string, int64) {}
func (silentRenderer) AssetProgress(string, int64)      {}
func (silentRenderer) AssetOK(*cob.AssetResult, string) {}
func (silentRenderer) AssetFail(string, string, error)  {}
func (silentRenderer) AssetSkipped(string)              {}
func (silentRenderer) Close()                           {}
