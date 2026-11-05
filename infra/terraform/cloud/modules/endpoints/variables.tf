variable "name" { type = string }
variable "region" { type = string }
variable "vpc_id" { type = string }
variable "private_subnets" { type = map(string) }
variable "route_table_ids" { type = list(string) }
variable "gateway_sg_id" { type = string }
variable "jobs_sg_id" { type = string }
variable "enable_interfaces" { type = bool }
variable "data_bucket_name" { type = string }
variable "table_arns" { type = list(string) }

variable "enable_edge" {
  type    = bool
  default = false
}
variable "edge_sg_id" {
  type    = string
  default = null
}
variable "edge_trust_anchor_arn" {
  type    = string
  default = null
}
variable "edge_certificate_common_name" {
  type    = string
  default = "miles-edge"
}
