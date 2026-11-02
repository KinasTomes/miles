variable "name" { type = string }
variable "bucket_name" { type = string }
variable "kms_key_arn" { type = string }

variable "max_request_units" {
  type        = number
  default     = 10
  description = "On-demand per-table request ceiling, not provisioned capacity or a billing hard cap."
  validation {
    condition     = var.max_request_units >= 1 && floor(var.max_request_units) == var.max_request_units
    error_message = "Use a positive integer request ceiling; unlimited capacity is not enabled."
  }
}
