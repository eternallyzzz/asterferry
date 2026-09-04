package domain

// CurrentControlProtocolVersion is shared by the JSON snapshot envelope,
// observed-state reports and the control-wire handshake. Database schema
// generations are separate and live in the Controller package.
const CurrentControlProtocolVersion = 1
