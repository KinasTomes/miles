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
variable "health_check_interval" {
  type        = number
  default     = 10
  description = "Target group health check interval in seconds (5-300)."
  validation {
    condition     = var.health_check_interval >= 5 && var.health_check_interval <= 300
    error_message = "Health check interval must be between 5 and 300 seconds."
  }
}
variable "health_check_timeout" {
  type        = number
  default     = 4
  description = "Target group health check timeout in seconds."
  validation {
    condition     = var.health_check_timeout >= 2 && var.health_check_timeout <= 60
    error_message = "Health check timeout must be between 2 and 60 seconds."
  }
}
variable "health_check_healthy_threshold" {
  type        = number
  default     = 2
  description = "Target group consecutive health check successes required before considering target healthy."
  validation {
    condition     = var.health_check_healthy_threshold >= 2 && var.health_check_healthy_threshold <= 10
    error_message = "Healthy threshold must be between 2 and 10."
  }
}
variable "health_check_unhealthy_threshold" {
  type        = number
  default     = 2
  description = "Target group consecutive health check failures required before considering target unhealthy."
  validation {
    condition     = var.health_check_unhealthy_threshold >= 2 && var.health_check_unhealthy_threshold <= 10
    error_message = "Unhealthy threshold must be between 2 and 10."
  }
}
