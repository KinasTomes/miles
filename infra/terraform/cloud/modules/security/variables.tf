variable "name" { type = string }
variable "region" { type = string }
variable "account_id" { type = string }
variable "autoscaling_role_arn" {
  type    = string
  default = null
}
