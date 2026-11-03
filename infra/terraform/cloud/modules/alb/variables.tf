variable "name" { type = string }
variable "enabled" { type = bool }
variable "vpc_id" { type = string }
variable "subnet_ids" { type = list(string) }
variable "security_group_id" { type = string }
variable "certificate_arn" { type = string }
variable "client_cidrs" { type = set(string) }
variable "deregistration_delay_seconds" {
  type        = number
  default     = 1800
  nullable    = false
  description = "ALB drain wait; capped by the existing application/lifecycle drain budget."
  validation {
    condition     = var.deregistration_delay_seconds >= 0 && var.deregistration_delay_seconds <= 1800 && floor(var.deregistration_delay_seconds) == var.deregistration_delay_seconds
    error_message = "ALB deregistration delay must be a whole number from 0 to 1800 seconds."
  }
}
