package expressdelivery

import maa "github.com/MaaXYZ/maa-framework-go/v4"

// Register registers express delivery custom components.
func Register() {
	maa.AgentServerRegisterCustomAction("ExpressDeliveryTraverseZiplineAction", &TraverseZiplineAction{})
	maa.AgentServerRegisterCustomAction("ExpressDeliveryRouteAction", &RouteAction{})
	maa.AgentServerRegisterCustomRecognition("ExpressDeliveryMinimapPresenceRecognition", &MinimapPresenceRecognition{})
}
