variable "name" { type = string }
variable "region" { type = string }
variable "account_id" { type = string }
variable "data_bucket_name" { type = string }
variable "table_arns" { type = list(string) }
variable "kms_key_arn" { type = string }
variable "secret_arn" { type = string }
variable "parameter_arn" { type = string }
variable "log_group_arn" { type = string }
variable "permissions_boundary_arn" {
  type    = string
  default = null
}
