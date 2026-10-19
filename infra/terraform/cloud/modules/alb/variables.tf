variable "name" { type = string }
variable "enabled" { type = bool }
variable "vpc_id" { type = string }
variable "subnet_ids" { type = list(string) }
variable "security_group_id" { type = string }
variable "certificate_arn" { type = string }
variable "client_cidrs" { type = set(string) }
