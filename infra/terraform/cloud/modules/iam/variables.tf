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
variable "fleet_enabled" {
  type    = bool
  default = false
}
variable "release_sha256" {
  type    = string
  default = null
}
variable "edge_ca_pem" {
  type        = string
  default     = null
  description = "Public CA certificate only. Private CA and leaf keys stay outside Terraform."
}

variable "runtime_iam_path" {
  type    = string
  default = "/miles/dev/"
}
variable "edge_certificate_common_name" {
  type    = string
  default = "miles-edge"
}

variable "enable_edge" {
  type    = bool
  default = false
}
variable "edge_bootstrap_secret_arn" {
  type    = string
  default = null
}
variable "edge_helper_sha256" {
  type    = string
  default = null
}
