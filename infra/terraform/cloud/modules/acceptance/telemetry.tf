# Run Command is restricted to this release-bundled diagnostic, never an
# operator-provided shell body. ENV_VAR requires a current SSM Agent.
resource "aws_ssm_document" "telemetry" {
  count           = var.enabled ? 1 : 0
  name            = "${var.name}-benchmark-telemetry"
  document_type   = "Command"
  document_format = "JSON"
  content = jsonencode({
    schemaVersion = "2.2"
    description   = "Bounded Miles Cloud metrics, OS and Chrony sampler"
    parameters = {
      action  = { type = "String", interpolationType = "ENV_VAR", allowedValues = ["run", "probe", "stop", "summary", "read"] }
      runid   = { type = "String", interpolationType = "ENV_VAR", allowedPattern = "^[a-z0-9-]{1,100}$" }
      slot    = { type = "String", interpolationType = "ENV_VAR", allowedPattern = "^[a-z0-9-]+-rep[1-3]$" }
      segment = { type = "String", interpolationType = "ENV_VAR", allowedPattern = "^[a-f0-9]{16}$" }
      source  = { type = "String", interpolationType = "ENV_VAR", allowedPattern = "^[a-f0-9]{40}$" }
      offset  = { type = "String", interpolationType = "ENV_VAR", allowedPattern = "^[0-9]{1,7}$" }
    }
    mainSteps = [{
      action = "aws:runShellScript"
      name   = "diagnostics"
      inputs = {
        timeoutSeconds = "1230"
        runCommand = [
          "set -eu",
          "python3 /opt/miles/current/scripts/cloud_node_telemetry.py \"$SSM_action\" \"$SSM_runid\" \"$SSM_slot\" \"$SSM_segment\" \"$SSM_source\" \"$SSM_offset\""
        ]
      }
    }]
  })
  tags = { Project = "Miles", Environment = var.environment, Installation = var.name, Owner = var.owner }
}

output "telemetry_document" {
  value = try({
    name      = aws_ssm_document.telemetry[0].name
    version   = aws_ssm_document.telemetry[0].latest_version
    hash      = aws_ssm_document.telemetry[0].hash
    hash_type = aws_ssm_document.telemetry[0].hash_type
  }, null)
}
