variable "name" { type = string }
variable "region" { type = string }
variable "vpc_id" { type = string }
variable "private_subnets" { type = map(string) }
variable "route_table_ids" { type = list(string) }
variable "gateway_sg_id" { type = string }
variable "jobs_sg_id" { type = string }
variable "enable_interfaces" { type = bool }
variable "enable_roles_anywhere" {
  type    = bool
  default = false
}
variable "roles_anywhere_trust_anchor_arn" {
  type    = string
  default = null
  validation {
    condition     = !var.enable_roles_anywhere || var.roles_anywhere_trust_anchor_arn != null
    error_message = "Roles Anywhere endpoint requires the trusted Edge anchor ARN."
  }
}
variable "data_bucket_name" { type = string }
variable "table_arns" { type = list(string) }
