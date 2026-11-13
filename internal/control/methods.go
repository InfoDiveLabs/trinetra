package control

// CoreMethods is every non-namespaced method dispatch handles, plus "Subscribe".
var CoreMethods = []string{
	"Snapshot", "Monitoring", "Series", "Events", "ActiveAlerts", "AlertHistory", "Config", "Doctor", "HostInfo",
	"Version", "ContainerLogs", "EnrollmentPIN", "MonitorTargets", "UpdateStatus", "UpdateCheck", "UpdateApply",
	"UpdateRollback", "ApplyConfig", "AckAlert", "UnackAlert", "TestChannel", "ValidateChannel", "Subscribe",
}

var fleetMethods = []string{
	"Fleet.Status", "Fleet.Nodes", "Fleet.RenameNode", "Fleet.SetNodeTags", "Fleet.SetNodeDeps",
	"Fleet.RevokeNode", "Fleet.RemoveNode", "Fleet.Tokens", "Fleet.CreateToken", "Fleet.DeleteToken",
	"Fleet.Incidents", "Fleet.Incident", "Fleet.AckIncident", "Fleet.Explain", "Fleet.Audit", "Fleet.Silences",
	"Fleet.CreateSilence", "Fleet.ExpireSilence", "Fleet.Maintenances", "Fleet.SaveMaintenance",
	"Fleet.DeleteMaintenance", "Fleet.Alerting", "Fleet.SetAlerting", "Fleet.RouteTest", "Fleet.RuleStates",
	"Fleet.Managed", "Fleet.SaveManaged", "Fleet.DeleteManaged", "Fleet.ManagedStatus", "Fleet.FleetSeries",
}

var statusPageMethods = []string{
	"StatusPage.Services", "StatusPage.SetService", "StatusPage.DeleteService", "StatusPage.Evaluation",
	"StatusPage.Incidents", "StatusPage.Incident", "StatusPage.CreateIncident", "StatusPage.PostUpdate",
	"StatusPage.EditUpdate", "StatusPage.EditIncident", "StatusPage.DeleteIncident", "StatusPage.Public",
}

// PluginsMethods is every Plugins.* method.
var PluginsMethods = []string{"Plugins.List", "Plugins.Get", "Plugins.Install", "Plugins.Enable", "Plugins.Disable",
	"Plugins.Remove", "Plugins.Update", "Plugins.SetConfig", "Plugins.Config", "Plugins.File", "Plugins.PackageFile",
	"Plugins.DevAttach", "Plugins.DevDetach", "Plugins.Slots", "Plugins.RenderSlot", "Plugins.InvokeAction",
	"Plugins.Catalog", "Plugins.Push", "Plugins.NodeStates"}
