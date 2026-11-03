variable "name" { type = string }
variable "region" { type = string }
variable "account_id" { type = string }
variable "autoscaling_role_arn" {
  type    = string
  default = null
}
variable "runtime_secret_name" {
  type        = string
  default     = null
  description = "Override only for a fresh installation when the original name is reserved by scheduled deletion."
}
