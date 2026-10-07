package proto

// Assignment 服务器下发给客户端的地址分配（ASSIGN 消息的 JSON body）。
type Assignment struct {
	Version   int      `json:"version"`
	LeaseID   uint64   `json:"lease_id"`
	IP        string   `json:"ip"`
	Prefix    int      `json:"prefix"`
	Gateway   string   `json:"gateway"`
	DNS       []string `json:"dns"`
	MTU       int      `json:"mtu"`
	MAC       string   `json:"mac"`
	ServerMAC string   `json:"server_mac"`
	ServerIP  string   `json:"server_ip"`
}

// Hello 客户端握手消息（HELLO 消息的 JSON body）。
type Hello struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
}
