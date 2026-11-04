// Тест API-клиента против настоящего сервера CloudBox (запускается в CI):
//   CB_URL=http://127.0.0.1:8080 CB_USER=andrey CB_PASS=<пароль приложения> flutter test test/live_test.dart
@TestOn('vm')
library;

import 'dart:io';
import 'dart:math';
import 'dart:typed_data';

import 'package:cloudbox/api.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  final url = Platform.environment['CB_URL'];
  final skip = url == null ? 'CB_URL не задан — живой тест пропущен' : null;

  test('файлы через API: папка, загрузка (в т.ч. частями), скачивание, переименование, корзина', () async {
    final api = CloudBoxApi(url!, Platform.environment['CB_USER']!, Platform.environment['CB_PASS']!);
    final me = await api.me();
    expect(me.username, Platform.environment['CB_USER']);

    final dir = await api.mkdir(null, 'Мобильное ${DateTime.now().millisecondsSinceEpoch}');
    final small = Uint8List.fromList('привет с телефона'.codeUnits);
    final s = await api.upload(parent: dir.id, name: 'заметка.txt', size: small.length, read: (o, l) async => small.sublist(o, o + l));
    final big = Uint8List(chunkSize + 12345);
    final rnd = Random(1);
    for (var i = 0; i < big.length; i++) {
      big[i] = rnd.nextInt(256);
    }
    final progress = <double>[];
    final b = await api.upload(parent: dir.id, name: 'большой.bin', size: big.length, read: (o, l) async => big.sublist(o, o + l), onProgress: progress.add);
    expect(b.size, big.length);
    expect(progress.length, greaterThan(1), reason: 'большой файл должен уйти частями');

    final back = <int>[];
    await for (final c in await api.download(b.id)) {
      back.addAll(c);
    }
    expect(back, big, reason: 'скачанный файл совпадает с загруженным побайтно');

    // автозагрузка кладёт фото в «Фото/Камера/<год>» — вложенные папки создаются сервером
    final p = await api.upload(parent: dir.id, name: 'IMG_0001.jpg', size: small.length, path: 'Фото/Камера/2026', read: (o, l) async => small.sublist(o, o + l));
    final again = await api.upload(parent: dir.id, name: 'IMG_0001.jpg', size: small.length, path: 'Фото/Камера/2026', read: (o, l) async => small.sublist(o, o + l));
    expect(again.name, isNot(p.name), reason: 'повтор с тем же именем не перезаписывает, а переименовывает');

    final l = await api.list(dir.id);
    expect(l.items.map((n) => n.name).toSet(), {'Фото', 'большой.bin', 'заметка.txt'});
    final r = await api.rename(s.id, 'заметка-2.txt');
    expect(r.name, 'заметка-2.txt');
    expect((await api.search('заметка-2')).any((n) => n.id == s.id), isTrue);
    await api.trash(dir.id);
    await expectLater(api.list(dir.id), throwsA(isA<ApiException>()));
    await expectLater(CloudBoxApi(url, 'никто', 'неверный').me(), throwsA(predicate((e) => e is ApiException && e.unauthorized)));
  }, skip: skip, timeout: const Timeout(Duration(minutes: 2)));
}
