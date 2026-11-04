import 'dart:convert';
import 'dart:typed_data';

import 'package:cloudbox/api.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

http.Response j(Object body, [int status = 200]) =>
    http.Response.bytes(utf8.encode(jsonEncode(body)), status, headers: {'content-type': 'application/json'});

Map<String, dynamic> node(String id, String name, {bool dir = false, int size = 0, bool e2e = false}) =>
    {'id': id, 'name': name, 'is_dir': dir, 'size': size, 'mime': dir ? '' : 'text/plain', 'updated_at': '2026-11-03T10:00:00Z', if (e2e) 'e2e': true};

void main() {
  test('адрес сервера нормализуется', () {
    expect(CloudBoxApi.normalizeServer('cloud.example.ru/').toString(), 'https://cloud.example.ru');
    expect(CloudBoxApi.normalizeServer(' http://192.168.1.5:8080// ').toString(), 'http://192.168.1.5:8080');
    expect(CloudBoxApi('http://x', 'u', 'p').insecure, isTrue);
    expect(() => CloudBoxApi.normalizeServer('https://'), throwsA(isA<ApiException>()));
  });

  test('Basic-авторизация с паролем приложения и разбор списка', () async {
    late http.Request seen;
    final api = CloudBoxApi('https://c.example', 'аня', 'abcde-12345', client: MockClient((r) async {
      seen = r;
      return j({'items': [node('1', 'Документы', dir: true), node('2', 'план.txt', size: 12), node('3', 'Сейф', dir: true, e2e: true)],
        'breadcrumbs': [], 'can_write': true});
    }));
    final l = await api.list(null);
    expect(seen.url.toString(), 'https://c.example/api/files');
    expect(seen.headers['Authorization'], 'Basic ${base64.encode(utf8.encode('аня:abcde-12345'))}');
    expect(l.items.map((n) => n.name), ['Документы', 'план.txt', 'Сейф']);
    expect(l.items[2].e2e, isTrue);
    expect(l.items[2].hasThumb, isFalse);
    await api.list('1');
    expect(seen.url.queryParameters['parent'], '1');
  });

  test('ошибки сервера — понятными сообщениями', () async {
    final api = CloudBoxApi('https://c', 'u', 'p', client: MockClient((r) async {
      if (r.url.path == '/api/me') return http.Response('', 401);
      return j({'error': 'превышена квота'}, 507);
    }));
    await expectLater(api.me(), throwsA(predicate((e) => e is ApiException && e.unauthorized && e.message.contains('пароль'))));
    await expectLater(api.mkdir(null, 'x'), throwsA(predicate((e) => e is ApiException && e.message == 'превышена квота')));
  });

  test('маленький файл — одним запросом с conflict=rename', () async {
    late http.Request seen;
    final api = CloudBoxApi('https://c', 'u', 'p', client: MockClient((r) async {
      seen = r;
      return j(node('9', 'фото.jpg', size: 3), 201);
    }));
    final data = Uint8List.fromList([1, 2, 3]);
    final n = await api.upload(parent: 'P', name: 'фото.jpg', size: 3, path: 'Фото/Камера/2026', read: (o, l) async => data.sublist(o, o + l));
    expect(n.id, '9');
    expect(seen.method, 'POST');
    expect(seen.url.path, '/api/files/upload');
    expect(seen.url.queryParameters, {'name': 'фото.jpg', 'parent': 'P', 'path': 'Фото/Камера/2026', 'conflict': 'rename'});
    expect(seen.bodyBytes, data);
  });

  test('большой файл — частями по 8 МБ, после обрыва продолжает с полученного', () async {
    const size = chunkSize * 2 + 100;
    final received = <int>[];
    var failedOnce = false;
    var got = 0;
    final api = CloudBoxApi('https://c', 'u', 'p', client: MockClient((r) async {
      switch ('${r.method} ${r.url.path}') {
        case 'POST /api/uploads':
          expect(jsonDecode(r.body)['size'], size);
          return j({'id': 'U', 'received': 0}, 201);
        case 'PUT /api/uploads/U':
          final off = int.parse(r.url.queryParameters['offset']!);
          expect(off, got, reason: 'клиент должен слать часть с того места, где остановился сервер');
          if (off == chunkSize && !failedOnce) {
            failedOnce = true; // сервер принял половину части и соединение оборвалось
            got += chunkSize ~/ 2;
            return j({'error': 'соединение прервалось', 'received': got}, 400);
          }
          got += r.bodyBytes.length;
          received.add(r.bodyBytes.length);
          return j({'received': got, 'size': size});
        case 'GET /api/uploads/U':
          return j({'id': 'U', 'received': got, 'size': size});
        case 'POST /api/uploads/U/complete':
          return j(node('F', 'видео.mp4', size: size), 201);
      }
      return http.Response('не ждали ${r.method} ${r.url}', 500);
    }));
    final progress = <double>[];
    final n = await api.upload(parent: null, name: 'видео.mp4', size: size, read: (o, l) async => Uint8List(l), onProgress: progress.add);
    expect(n.id, 'F');
    expect(got, size);
    expect(failedOnce, isTrue);
    expect(progress.last, 1.0);
  }, timeout: const Timeout(Duration(seconds: 30)));

  test('размеры', () {
    expect(formatSize(512), '512 Б');
    expect(formatSize(1536), '1.5 КБ');
    expect(formatSize(20 * 1024 * 1024), '20 МБ');
  });
}
