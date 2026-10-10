package testjsonl

// ChatGPTImageExport returns an export with one conversation and its image.
func ChatGPTImageExport() map[string]string {
	return map[string]string{
		"conversations-000.json": `[{"id":"cg-1","conversation_id":"cg-1","title":"Import",
  "create_time":1706745600.0,"update_time":1706745660.0,"current_node":"n1","mapping":{
    "r":{"id":"r","parent":null,"children":["n1"],"message":null},
    "n1":{"id":"n1","parent":"r","children":[],"message":{"id":"m1","create_time":1706745600.0,
      "author":{"role":"user","name":null,"metadata":{}},
      "content":{"content_type":"multimodal_text","parts":["See this:",
        {"content_type":"image_asset_pointer","asset_pointer":"file-service://file-img1"}]},
      "status":"finished_successfully","metadata":{}}}}}]`,
		"file-img1-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee.png": "\x89PNG\r\n\x1a\n",
	}
}
