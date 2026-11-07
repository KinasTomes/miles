variable "name" { type = string }
variable "table_prefix" { type = string }
variable "enabled" { type = bool }
variable "profiles" {
  type    = set(string)
  default = ["cloud", "edge"]
  validation {
    condition     = length(var.profiles) > 0 && alltrue([for p in var.profiles : contains(["cloud", "edge"], p)])
    error_message = "Monitor only the Cloud/Edge profiles present in this installation."
  }
}
variable "alarm_actions" {
  type    = list(string)
  default = []
  validation {
    condition     = length(var.alarm_actions) <= 5 && alltrue([for arn in var.alarm_actions : can(regex("^arn:aws:sns:[a-z0-9-]+:[0-9]{12}:[A-Za-z0-9_-]+$", arn))])
    error_message = "Use up to five SNS topic ARNs; runtime alarms never stop or replace instances."
  }
}
variable "deep_observation" {
  type        = bool
  default     = false
  description = "Enable alarms for metrics collected only during deep DynamoDB table scans (e.g. miles_stuck_completing_uploads)."
}
