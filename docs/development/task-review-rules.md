# Quy tắc review task và PR

Áp dụng khi review lần đầu, review commit sửa lỗi và kiểm tra điều kiện đóng task. Ưu tiên chỉ dẫn hiện tại của người dùng, sau đó đến quy tắc repository và tài liệu này.

## 1. Chốt đúng scope trước khi đọc diff

- Xác định task ID, đầu ra, acceptance criteria, dependency và phần được giao cho task sau. Đọc task gốc và contract/API/schema liên quan; không chỉ dựa vào mô tả PR.
- Kiểm tra trạng thái backlog hiện hành. File backlog cũ hoặc trạng thái “Done” là thông tin đối chiếu, không thay bằng chứng bàn giao.
- Phân biệt ba câu hỏi: PR có hoàn thành phần được giao không, có làm hỏng phần đang dùng không, và có buộc task khác phải đổi theo không.
- Không yêu cầu task làm luôn tính năng của tuần sau. Tuy nhiên, lỗi bảo mật, mất dữ liệu hoặc contract sai phải được báo kể cả khi hậu quả xuất hiện ở downstream.

## 2. Review hành vi và ảnh hưởng downstream

- Đọc cả caller, implementation liên quan, test và fixture; không chỉ đọc các dòng thay đổi.
- Kiểm tra đường chạy thành công, lỗi, boundary, quyền, dữ liệu không tin cậy, concurrency và tương thích theo mức rủi ro thực tế của thay đổi.
- Với interface/schema/serialization, đối chiếu producer và consumer: tên, kiểu, giá trị mặc định, version, checksum, giới hạn và quy tắc xử lý lỗi. Không để mỗi module hiểu một kiểu.
- Mỗi ảnh hưởng downstream phải nêu rõ module/task nào bị ảnh hưởng, tình huống kích hoạt, hậu quả và việc cần sửa hoặc chốt trước khi tích hợp.
- Không suy diễn rằng phải chờ toàn bộ một tuần. Chỉ ra đúng dependency hoặc đầu ra còn thiếu, phần nào vẫn làm được ngay và phần nào chưa thể nghiệm thu.
- Chạy kiểm tra nhỏ nhất đủ chứng minh hành vi. Ghi rõ kiểm tra nào đã chạy, chưa chạy và giới hạn của mock/local test so với tích hợp thật.

## 3. Chỉ báo vấn đề có căn cứ

- Finding cần có vị trí code, điều kiện kích hoạt, hành vi sai, tác động và hướng sửa đủ rõ. Phân biệt lỗi đã chứng minh với nghi vấn cần xác minh.
- Dùng mức ưu tiên theo tác động: **P0** sự cố nghiêm trọng cần xử lý ngay; **P1** lỗi nghiêm trọng cần sửa trước khi merge; **P2** lỗi có điều kiện cụ thể cần sửa; **P3** cải thiện nhỏ.
- Không biến sở thích cá nhân hay đề xuất mở rộng thành blocker. Nếu chỉ là gợi ý, ghi rõ không chặn merge.
- Không lặp nhiều comment cho cùng một nguyên nhân. Không báo lỗi đã sửa ở head hiện tại.

## 4. Báo lại trước khi gửi lên GitHub

Khi được yêu cầu “review”, mặc định kiểm tra và báo kết quả trong cuộc trò chuyện trước. Chỉ gửi comment, request changes, approve hoặc merge khi đã có chỉ dẫn tương ứng; chỉ dẫn đã được cho phép trong phiên vẫn có hiệu lực, không hỏi lại vô ích.

Báo cáo ngắn, theo thứ tự:

1. Kết luận về scope và mức độ hoàn thành task.
2. Findings theo mức ưu tiên, kèm bằng chứng.
3. Ảnh hưởng downstream và các quyết định cần chốt, nếu có.
4. Kiểm tra đã chạy, giới hạn còn lại và đề xuất bước tiếp theo.

Nếu không có finding, nói rõ và nêu giới hạn kiểm chứng. Không khẳng định hệ thống chạy đúng chỉ vì CI xanh.

## 5. Xử lý open question và đề xuất của tác giả

- Đọc cả discussion, review thread và đề xuất mới của tác giả; không chỉ xem commit.
- Quyết định kỹ thuật thông thường trong scope có thể chốt khi đã được giao quyền. Dựa vào contract hiện hành và ghi rõ kết luận để downstream dùng chung.
- Với lựa chọn làm đổi public contract, quyền truy cập, dữ liệu, chi phí hoặc khó đảo ngược, trình tác động và phương án cụ thể để người dùng quyết định, trừ khi đã có chỉ dẫn đủ rõ.
- Khi chấp nhận đề xuất, kiểm tra code, tài liệu và test có phản ánh quyết định đó chưa. Phân biệt “đồng ý hướng xử lý” với “đã triển khai xong”.

## 6. Review lại sau commit mới

- Lấy head mới nhất và đối chiếu từng finding cũ với thay đổi thực tế. Kiểm tra cả lỗi phát sinh do bản sửa.
- Chạy lại kiểm tra bị ảnh hưởng; không chạy lặp kiểm tra không liên quan khi không có lý do mới.
- Báo rõ từng vấn đề đã sửa, còn thiếu hoặc được thay bằng quyết định nào. Chỉ resolve thread khi nội dung đã được xử lý.
- Kết luận review phải gắn với head đã kiểm tra. Có commit mới sau đó thì xem lại phần thay đổi trước khi approve hoặc merge.

## 7. Comment, approve, merge và cập nhật task

- Viết bằng vai trò của chủ tài khoản: trực tiếp, ngắn, rõ việc cần làm. Không ghi “yêu cầu của Hưng”, “theo yêu cầu của Hưng” hay kể chuyện giao việc cho trợ lý.
- Tên file và branch phải mô tả chức năng hoặc vấn đề; không đặt theo `w1`, `w2` hay tiền tố `codex/`. Task ID chỉ nằm trong PR, issue hoặc backlog khi cần truy vết; không đưa vào tài liệu.
- Khi được yêu cầu sửa code, chia commit theo phần thay đổi có ý nghĩa, tránh gom mọi phần vào một commit.
- Khi được yêu cầu “approve rồi merge”, gửi approve message ngắn sau khi kiểm tra xong, rồi merge nếu required checks, approval và discussion đáp ứng quy định repository.
- Không tự bỏ qua bảo vệ nhánh. Quyền admin merge chỉ áp dụng cho PR và điều kiện đã được người dùng cho phép; không suy rộng từ lần trước.
- Sau merge, xác nhận kết quả thực tế. Nếu được giao cập nhật backlog, điền PR, bằng chứng và phần còn thiếu; không chỉ đổi nhãn trạng thái.
- **Merged không tự đồng nghĩa Done.** Chỉ đóng task khi acceptance criteria đạt và có bằng chứng phù hợp. IaC plan/mock test chứng minh phần chuẩn bị; không thay bằng chứng apply, tích hợp, vận hành hoặc thu hồi thật nếu task yêu cầu.
- Với hạ tầng, tách rõ “chuẩn bị IaC”, “plan/test”, “apply AWS” và “teardown”. Chỉ plan/test không cho phép tự tạo tài nguyên tính phí; không đưa bí mật hay định danh hạ tầng riêng lên PR công khai.

## 8. Không đưa ID task vào tài liệu

- Không đưa ID task vào tên file, tiêu đề, nội dung, bảng, nhãn hiện thực, liên kết hay code block của tài liệu trong repository. Áp dụng cả README, API, kiến trúc, contract, hướng dẫn, kế hoạch và tài liệu kiểm thử/validation.
- Tài liệu mô tả chức năng, contract, module, trạng thái hỗ trợ và bằng chứng kỹ thuật. Dependency hoặc phần chưa triển khai phải nêu rõ hành vi/module còn thiếu; không dùng mã task, tên người được giao hay trạng thái backlog thay lời giải thích kỹ thuật.
- Truy vết công việc và phân công để trong mô tả PR, issue hoặc backlog. Có thể dẫn PR/commit hoặc tài liệu kỹ thuật theo chủ đề, nhưng nhãn và URL trong tài liệu không chứa ID task.
- Rà cả tên file và phần nội dung thêm/sửa trong diff. ID task mới trong tài liệu là finding phải sửa trước merge, kể cả khi CI chỉ kiểm tên file và đang xanh. Không yêu cầu PR sửa toàn bộ tài liệu cũ ngoài scope; ghi riêng phần tồn đọng để dọn sau.
